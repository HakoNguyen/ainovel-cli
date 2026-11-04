package web

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/voocel/ainovel-cli/assets"
	"github.com/voocel/ainovel-cli/internal/bootstrap"
	"github.com/voocel/ainovel-cli/internal/host"
	"github.com/voocel/ainovel-cli/internal/store"
)

// library 管理书库里每本书的运行时。每本书一个独立 Host（各自的目录租约、store、模型用量），
// 因此多本书可以同时创作。Host 按需打开，打开后常驻直到进程退出或书被移出书库。
type library struct {
	cfg  bootstrap.Config // 基础配置；每本书只替换 OutputDir
	db   *db
	root string // 新书目录的父目录

	mu   sync.Mutex
	open map[string]*bookRT
}

// bookRT 是一本已打开的书：Host + 只读 store + 事件扇出。
// 配置变更时整份替换成新的 bookRT（新 Host），hub 沿用，浏览器的事件连接不断。
type bookRT struct {
	id    string
	eng   *host.Host
	store *store.Store
	hub   *hub
	cfg   bootstrap.Config
	stale bool      // 配置已变更，等引擎停下后用新配置重开；仅在 library.mu 下读写
	guard loopGuard // 复读检测；只在 pump goroutine 内使用
}

func newLibrary(cfg bootstrap.Config, d *db, root string) *library {
	return &library{cfg: cfg, db: d, root: root, open: map[string]*bookRT{}}
}

// registerLegacy 把 TUI 默认输出目录里已有的作品登记进书库（只登记，不移动文件）。
func (l *library) registerLegacy() error {
	dir := filepath.Clean(l.cfg.OutputDir)
	if exists, err := l.db.hasBookDir(dir); err != nil || exists {
		return err
	}
	s := store.NewStore(dir)
	book, _ := s.Book.Load()
	premise, _ := s.Outline.LoadPremise()
	if book == nil && premise == "" {
		return nil
	}
	name := "Truyện từ TUI"
	if book != nil && book.Title != "" {
		name = book.Title
	}
	_, err := l.db.addBook(newID(), name, dir)
	return err
}

// create 新建一本空书：分配目录并登记。真正的创作由 start 发起。
func (l *library) create(name string) (bookRow, error) {
	id := newID()
	dir := filepath.Join(l.root, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return bookRow{}, err
	}
	name = strings.TrimSpace(name)
	if name == "" {
		name = "Truyện chưa đặt tên"
	}
	return l.db.addBook(id, name, dir)
}

// runtime 返回已打开的书，必要时打开它。目录被别的进程（如 TUI）占用时返回错误。
// 配置变更后的书会在引擎空闲时于此处换成新 Host。
func (l *library) runtime(id string) (*bookRT, error) {
	row, err := l.db.getBook(id)
	if err != nil {
		return nil, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	rt, ok := l.open[id]
	if ok && !rt.stale {
		return rt, nil
	}
	var shared *hub
	if ok {
		if rt.eng.Snapshot().IsRunning {
			return rt, nil
		}
		rt.eng.Close()
		delete(l.open, id)
		shared = rt.hub
	}
	return l.openLocked(row, shared)
}

func (l *library) openLocked(row bookRow, shared *hub) (*bookRT, error) {
	cfg := l.cfg
	cfg.OutputDir = row.Dir
	bundle := assets.LoadWithLanguage(cfg.NormalizedLanguage(), cfg.Style, assets.DefaultLoadOptions(cfg.OutputDir))
	eng, err := host.New(cfg, bundle)
	if err != nil {
		return nil, fmt.Errorf("không mở được truyện “%s”: %w", row.Name, err)
	}
	if shared == nil {
		shared = newHub()
	}
	rt := &bookRT{id: row.ID, eng: eng, store: store.NewStore(eng.Dir()), hub: shared, cfg: cfg}
	go rt.pump()
	l.open[row.ID] = rt
	return rt, nil
}

// config 返回当前生效配置的副本。
func (l *library) config() bootstrap.Config {
	l.mu.Lock()
	defer l.mu.Unlock()
	return bootstrap.CloneConfig(l.cfg)
}

// applyConfig 校验并保存新配置，然后让已打开的书用新配置重开：
// 空闲的立即重开，正在运行的标记为过期，等它停下后由 runtime 重开。
func (l *library) applyConfig(cfg bootstrap.Config) error {
	if err := cfg.ValidateBase(); err != nil {
		return err
	}
	if _, err := bootstrap.NewModelSet(cfg); err != nil {
		return fmt.Errorf("không tạo được model: %w", err)
	}
	if err := bootstrap.SaveConfig(bootstrap.EffectiveConfigPath(), cfg); err != nil {
		return fmt.Errorf("lưu cấu hình thất bại: %w", err)
	}
	l.mu.Lock()
	l.cfg = cfg
	var ids []string
	for id, rt := range l.open {
		rt.stale = true
		ids = append(ids, id)
	}
	l.mu.Unlock()
	for _, id := range ids {
		if _, err := l.runtime(id); err != nil {
			slog.Warn("web: mở lại truyện sau khi đổi cấu hình thất bại", "module", "web", "book", id, "err", err)
		}
	}
	return nil
}

func (l *library) runningCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, rt := range l.open {
		if rt.eng.Snapshot().IsRunning {
			n++
		}
	}
	return n
}

// peek 返回已打开的运行时（不触发打开），用于书库列表显示实时状态。
func (l *library) peek(id string) *bookRT {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.open[id]
}

func (l *library) closeBook(id string) {
	l.mu.Lock()
	rt := l.open[id]
	delete(l.open, id)
	l.mu.Unlock()
	if rt != nil {
		rt.eng.Close()
	}
}

func (l *library) closeAll() {
	l.mu.Lock()
	open := l.open
	l.open = map[string]*bookRT{}
	l.mu.Unlock()
	var wg sync.WaitGroup
	for _, rt := range open {
		wg.Add(1)
		go func(rt *bookRT) {
			defer wg.Done()
			rt.eng.Abort()
			rt.eng.Close()
		}(rt)
	}
	wg.Wait()
}

func newID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// stopLooping 在模型复读时暂停引擎：继续生成只会烧到 max_tokens，结果也用不了。
func (rt *bookRT) stopLooping(sample string) {
	slog.Warn("web: model lặp vô nghĩa, tự dừng engine", "module", "web", "book", rt.id, "sample", sample)
	go rt.eng.Abort()
	rt.hub.publishError(fmt.Sprintf("Model đang lặp lại vô nghĩa (…%s…) nên đã tự dừng để khỏi tốn tiền. "+
		"Hãy đổi model cho Kiến trúc sư / Người viết trong ⚙ Cài đặt rồi bấm ▶ Tiếp tục.", strings.TrimSpace(sample)))
}

// pump 把 Host 的事件与流式输出转发到这本书的所有浏览器连接。
func (rt *bookRT) pump() {
	for {
		select {
		case ev, ok := <-rt.eng.Events():
			if !ok {
				return
			}
			if strings.TrimSpace(ev.Summary) == "" {
				continue
			}
			rt.hub.publishEvent(eventPayload{
				Time: ev.Time.Format("15:04:05"), Category: ev.Category,
				Summary: ev.Summary, Detail: ev.Detail, Level: ev.Level,
			})
		case delta, ok := <-rt.eng.Stream():
			if !ok {
				return
			}
			if delta == host.StreamClearSentinel {
				rt.guard.reset()
				rt.hub.publishClear()
				continue
			}
			if delta != "" {
				rt.hub.publishDelta(delta)
				if tripped, sample := rt.guard.feed(delta); tripped {
					rt.stopLooping(sample)
				}
			}
		case _, ok := <-rt.eng.Done():
			if !ok {
				return
			}
			rt.guard.reset()
			rt.hub.publish("done", nil)
		}
	}
}
