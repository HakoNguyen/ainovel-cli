// Package web 提供浏览器界面：书库 + 每本书的写作台（左栏大纲/设定，中间 AI 稿与作者稿并排，右栏对话助手）。
// 它与 TUI / headless 一样只是 Host 的消费者，不改动创作引擎的任何规则。
package web

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/voocel/ainovel-cli/internal/bootstrap"
	"github.com/voocel/ainovel-cli/internal/domain"
	"github.com/voocel/ainovel-cli/internal/entry/startup"
	runtimelog "github.com/voocel/ainovel-cli/internal/logger"
	"github.com/voocel/ainovel-cli/internal/store"
)

//go:embed static
var staticFS embed.FS

// 作者自己的稿子放在作品目录的独立子目录里，引擎从不读写这里，
// 因此不会触发章节修订检测，也不会被 AI 的提交覆盖。
const userDraftDir = "user_drafts"

type Options struct {
	Addr      string
	DBPath    string // 默认 ainovel.db（当前目录）
	BooksRoot string // 默认 books（当前目录）
}

type server struct {
	lib *library
	db  *db
	cfg bootstrap.Config
}

// Run 启动网页服务并阻塞到收到中断信号。
func Run(cfg bootstrap.Config, opts Options) error {
	if opts.DBPath == "" {
		opts.DBPath = "ainovel.db"
	}
	if opts.BooksRoot == "" {
		opts.BooksRoot = "books"
	}
	// 多本书共用一个进程，slog 默认 logger 是全局的：统一写到一个文件，
	// 而不是让每个 Host 各自替换默认 logger。
	if cleanup, err := runtimelog.SetupFile(".", "web.log", false); err == nil {
		defer cleanup()
	} else {
		fmt.Fprintf(os.Stderr, "Cảnh báo: không mở được file log: %v\n", err)
	}

	d, err := openDB(opts.DBPath)
	if err != nil {
		return err
	}
	defer d.Close()
	lib := newLibrary(cfg, d, opts.BooksRoot)
	if err := lib.registerLegacy(); err != nil {
		slog.Warn("web: đăng ký truyện cũ thất bại", "module", "web", "err", err)
	}
	defer lib.closeAll()
	s := &server{lib: lib, db: d, cfg: cfg}

	static, err := fs.Sub(staticFS, "static")
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	files := http.FileServer(http.FS(static))
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		// 静态文件随二进制一起更新，让浏览器每次都重新校验，免得升级后还跑旧脚本。
		w.Header().Set("Cache-Control", "no-cache")
		files.ServeHTTP(w, r)
	})

	mux.HandleFunc("GET /api/books", s.handleListBooks)
	mux.HandleFunc("POST /api/books", s.handleCreateBook)
	mux.HandleFunc("PATCH /api/books/{id}", s.handleRenameBook)
	mux.HandleFunc("DELETE /api/books/{id}", s.handleArchiveBook)

	mux.HandleFunc("GET /api/books/{id}/events", s.withBook(s.handleEvents))
	mux.HandleFunc("GET /api/books/{id}/state", s.withBook(s.handleState))
	mux.HandleFunc("GET /api/books/{id}/outline", s.withBook(s.handleOutline))
	mux.HandleFunc("GET /api/books/{id}/chapter/{n}", s.withBook(s.handleChapter))
	mux.HandleFunc("PUT /api/books/{id}/chapter/{n}/mine", s.withBook(s.handleSaveMine))
	mux.HandleFunc("GET /api/books/{id}/export", s.withBook(s.handleExportMine))
	mux.HandleFunc("POST /api/books/{id}/images", s.handleUploadImage)
	mux.HandleFunc("GET /api/books/{id}/images/{name}", s.handleGetImage)
	mux.HandleFunc("POST /api/books/{id}/start", s.withBook(s.handleStart))
	mux.HandleFunc("POST /api/books/{id}/resume", s.withBook(s.handleResume))
	mux.HandleFunc("POST /api/books/{id}/pause", s.withBook(s.handlePause))
	mux.HandleFunc("POST /api/books/{id}/next", s.withBook(s.handleNext))
	mux.HandleFunc("POST /api/books/{id}/mode", s.withBook(s.handleMode))
	mux.HandleFunc("POST /api/books/{id}/steer", s.withBook(s.handleSteer))
	mux.HandleFunc("POST /api/books/{id}/chat", s.withBook(s.handleChat))

	mux.HandleFunc("GET /api/settings", s.handleGetSettings)
	mux.HandleFunc("GET /api/settings/catalog", s.handleCatalog)
	mux.HandleFunc("PUT /api/settings/provider", s.handleSaveProvider)
	mux.HandleFunc("POST /api/settings/test", s.handleTestProvider)
	mux.HandleFunc("PUT /api/settings/models", s.handleSaveModels)
	mux.HandleFunc("GET /api/settings/openrouter", s.handleOpenRouterCredits)

	mux.HandleFunc("GET /api/books/{id}/sessions", s.handleListSessions)
	mux.HandleFunc("POST /api/books/{id}/sessions", s.handleCreateSession)
	mux.HandleFunc("GET /api/sessions/{sid}/messages", s.handleListMessages)
	mux.HandleFunc("PATCH /api/sessions/{sid}", s.handleRenameSession)
	mux.HandleFunc("DELETE /api/sessions/{sid}", s.handleDeleteSession)

	srv := &http.Server{Addr: opts.Addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	fmt.Fprintf(os.Stderr, "ainovel web đang chạy tại %s (CSDL: %s, thư mục truyện: %s)\n", opts.Addr, opts.DBPath, opts.BooksRoot)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	select {
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-sig:
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return srv.Shutdown(ctx)
	}
}

type bookHandler func(w http.ResponseWriter, r *http.Request, rt *bookRT)

// withBook 解析路径里的书 ID 并按需打开该书的 Host。
func (s *server) withBook(h bookHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rt, err := s.lib.runtime(r.PathValue("id"))
		if errors.Is(err, errNotFound) {
			writeErr(w, http.StatusNotFound, fmt.Errorf("Không tìm thấy truyện"))
			return
		}
		if err != nil {
			writeErr(w, http.StatusConflict, err)
			return
		}
		h(w, r, rt)
	}
}

// ---- 书库 ----

type bookSummary struct {
	bookRow
	Title     string  `json:"title"`
	Open      bool    `json:"open"`
	Running   bool    `json:"running"`
	State     string  `json:"state"`
	Phase     string  `json:"phase"`
	Mode      string  `json:"mode"`
	Completed int     `json:"completed"`
	Total     int     `json:"total"`
	CostUSD   float64 `json:"cost_usd"`
	MineCount int     `json:"mine_chapters"`
	MineWords int     `json:"mine_words"`
	Synopsis  string  `json:"synopsis"`
}

func (s *server) handleListBooks(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.listBooks()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	out := []bookSummary{}
	for _, row := range rows {
		out = append(out, s.summarize(row))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *server) summarize(row bookRow) bookSummary {
	sum := bookSummary{bookRow: row, Title: row.Name}
	st := store.NewStore(row.Dir)
	if book, _ := st.Book.Load(); book != nil {
		if book.Title != "" {
			sum.Title = book.Title
		}
		sum.Synopsis = clip(book.Synopsis, 220)
	}
	if rt := s.lib.peek(row.ID); rt != nil {
		snap := rt.eng.Snapshot()
		sum.Open = true
		sum.Running = snap.IsRunning || snap.RuntimeState == "running"
		sum.State, sum.Phase, sum.Mode = snap.RuntimeState, snap.Phase, snap.AdvanceMode
		sum.Completed, sum.Total, sum.CostUSD = snap.CompletedCount, snap.TotalChapters, snap.TotalCostUSD
	} else {
		if p, _ := st.Progress.Load(); p != nil {
			sum.Phase, sum.Completed, sum.Total = string(p.Phase), len(p.CompletedChapters), p.TotalChapters
		}
		if m, _ := st.RunMeta.Load(); m != nil {
			sum.Mode = string(m.AdvanceMode)
		}
	}
	sum.MineCount, sum.MineWords = mineStats(row.Dir)
	return sum
}

func (s *server) handleCreateBook(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name   string `json:"name"`
		Prompt string `json:"prompt"`
		Mode   string `json:"mode"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.Prompt) == "" {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("Hãy nhập ý tưởng truyện"))
		return
	}
	prompt, err := startup.PrepareQuick(body.Prompt)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	row, err := s.lib.create(body.Name)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	rt, err := s.lib.runtime(row.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if err := s.startBook(rt, prompt, body.Mode); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusCreated, row)
}

func (s *server) handleRenameBook(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("Tên trống"))
		return
	}
	if err := s.db.renameBook(r.PathValue("id"), name); err != nil {
		writeErr(w, statusOf(err), err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *server) handleArchiveBook(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if rt := s.lib.peek(id); rt != nil && rt.eng.Snapshot().IsRunning {
		writeErr(w, http.StatusConflict, fmt.Errorf("AI đang chạy truyện này, hãy dừng trước"))
		return
	}
	if err := s.db.archiveBook(id); err != nil {
		writeErr(w, statusOf(err), err)
		return
	}
	s.lib.closeBook(id)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---- 读取 ----

func (s *server) handleState(w http.ResponseWriter, r *http.Request, rt *bookRT) {
	snap := rt.eng.Snapshot()
	book, _ := rt.store.Book.Load()
	row, _ := s.db.getBook(rt.id)
	writeJSON(w, http.StatusOK, map[string]any{
		"snapshot": snap,
		"name":     row.Name,
		"has_book": book != nil || snap.Premise != "" || len(snap.Outline) > 0,
		"language": s.cfg.NormalizedLanguage(),
	})
}

type chapterItem struct {
	Chapter   int    `json:"chapter"`
	Title     string `json:"title"`
	CoreEvent string `json:"core_event"`
	AIDone    bool   `json:"ai_done"`
	MineWords int    `json:"mine_words"`
}

type chapterGroup struct {
	Volume   string        `json:"volume"`
	Arc      string        `json:"arc"`
	Goal     string        `json:"goal"`
	Pending  int           `json:"pending"` // 尚未展开的骨架弧预估章数
	Chapters []chapterItem `json:"chapters"`
}

func (s *server) handleOutline(w http.ResponseWriter, r *http.Request, rt *bookRT) {
	st := rt.store
	book, _ := st.Book.Load()
	premise, _ := st.Outline.LoadPremise()
	chars, _ := st.Characters.Load()
	world, _ := os.ReadFile(filepath.Join(st.Dir(), "world_rules.md"))
	progress, _ := st.Progress.Load()

	done := map[int]bool{}
	if progress != nil {
		for _, ch := range progress.CompletedChapters {
			done[ch] = true
		}
	}
	item := func(e domain.OutlineEntry) chapterItem {
		return chapterItem{Chapter: e.Chapter, Title: e.Title, CoreEvent: e.CoreEvent,
			AIDone: done[e.Chapter], MineWords: countWords(loadMine(st.Dir(), e.Chapter))}
	}

	var groups []chapterGroup
	if volumes, _ := st.Outline.LoadLayeredOutline(); len(volumes) > 0 {
		n := 1
		for _, v := range volumes {
			if len(v.Arcs) == 0 {
				groups = append(groups, chapterGroup{Volume: v.Title, Goal: v.Theme})
				continue
			}
			for _, a := range v.Arcs {
				g := chapterGroup{Volume: v.Title, Arc: a.Title, Goal: a.Goal, Pending: a.EstimatedChapters}
				for _, e := range a.Chapters {
					e.Chapter = n
					g.Chapters = append(g.Chapters, item(e))
					n++
				}
				groups = append(groups, g)
			}
		}
	} else if entries, _ := st.Outline.LoadOutline(); len(entries) > 0 {
		g := chapterGroup{}
		for _, e := range entries {
			g.Chapters = append(g.Chapters, item(e))
		}
		groups = append(groups, g)
	}

	resp := map[string]any{
		"premise":    premise,
		"characters": chars,
		"world":      string(world),
		"groups":     groups,
	}
	if book != nil {
		resp["title"] = book.Title
		resp["synopsis"] = book.Synopsis
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *server) handleChapter(w http.ResponseWriter, r *http.Request, rt *bookRT) {
	n, ok := chapterParam(w, r)
	if !ok {
		return
	}
	plan, _ := rt.store.Drafts.LoadChapterPlan(n)
	aiText, _ := rt.store.Drafts.LoadChapterText(n)
	aiState := "final"
	if aiText == "" {
		aiText, _ = rt.store.Drafts.LoadDraft(n)
		aiState = "draft"
		if aiText == "" {
			aiState = "none"
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"chapter":   n,
		"outline":   outlineEntry(rt.store, n),
		"plan":      plan,
		"ai_text":   aiText,
		"ai_state":  aiState,
		"mine":      loadMine(rt.store.Dir(), n),
		"mine_html": loadMineHTML(rt.store.Dir(), n),
	})
}

func outlineEntry(st *store.Store, n int) *domain.OutlineEntry {
	if e, err := st.Outline.GetChapterOutline(n); err == nil {
		return e
	}
	if e, err := st.Outline.GetChapterFromLayered(n); err == nil {
		return e
	}
	return nil
}

// ---- 作者稿 ----

// 每章两份：NN.html 是编辑器里带格式的原稿，NN.md 是同步写出的纯文本，
// 供字数统计、助手上下文和导出使用（服务端无需解析 HTML）。
func minePath(dir string, n int) string {
	return filepath.Join(dir, userDraftDir, fmt.Sprintf("%02d.md", n))
}

func mineHTMLPath(dir string, n int) string {
	return filepath.Join(dir, userDraftDir, fmt.Sprintf("%02d.html", n))
}

func loadMineHTML(dir string, n int) string {
	data, err := os.ReadFile(mineHTMLPath(dir, n))
	if err != nil {
		return ""
	}
	return string(data)
}

func writeAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func loadMine(dir string, n int) string {
	data, err := os.ReadFile(minePath(dir, n))
	if err != nil {
		return ""
	}
	return string(data)
}

// mineChapters 列出作者已写过的章节号（升序）。
func mineChapters(dir string) []int {
	files, _ := os.ReadDir(filepath.Join(dir, userDraftDir))
	var chapters []int
	for _, f := range files {
		name := f.Name()
		if !strings.HasSuffix(name, ".md") {
			continue
		}
		if n, err := strconv.Atoi(strings.TrimSuffix(name, ".md")); err == nil {
			chapters = append(chapters, n)
		}
	}
	sort.Ints(chapters)
	return chapters
}

func mineStats(dir string) (chapters, words int) {
	for _, n := range mineChapters(dir) {
		if wc := countWords(loadMine(dir, n)); wc > 0 {
			chapters++
			words += wc
		}
	}
	return chapters, words
}

func (s *server) handleSaveMine(w http.ResponseWriter, r *http.Request, rt *bookRT) {
	n, ok := chapterParam(w, r)
	if !ok {
		return
	}
	var body struct {
		Text string `json:"text"`
		HTML string `json:"html"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	dir := rt.store.Dir()
	if err := writeAtomic(minePath(dir, n), []byte(body.Text)); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if err := writeAtomic(mineHTMLPath(dir, n), []byte(body.HTML)); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	s.db.touchBook(rt.id)
	writeJSON(w, http.StatusOK, map[string]any{"words": countWords(body.Text)})
}

func (s *server) handleExportMine(w http.ResponseWriter, r *http.Request, rt *bookRT) {
	dir := rt.store.Dir()
	var b strings.Builder
	title := rt.id
	if book, _ := rt.store.Book.Load(); book != nil && book.Title != "" {
		title = book.Title
		b.WriteString(book.Title + "\n\n")
	}
	for _, n := range mineChapters(dir) {
		text := strings.TrimSpace(loadMine(dir, n))
		if text == "" {
			continue
		}
		heading := fmt.Sprintf("Chương %d", n)
		if e := outlineEntry(rt.store, n); e != nil && e.Title != "" {
			heading += ": " + e.Title
		}
		b.WriteString(heading + "\n\n" + text + "\n\n\n")
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="ban-thao.txt"; filename*=UTF-8''%s.txt`, urlEscape(title)))
	_, _ = w.Write([]byte(b.String()))
}

// ---- 控制 ----

func modeOf(s string) domain.ChapterAdvanceMode {
	if s == "auto" {
		return domain.ChapterAdvanceAuto
	}
	return domain.ChapterAdvanceReview
}

// startBook 设置推进模式后在后台发起启动裁定；结果和错误都经事件流回到页面。
func (s *server) startBook(rt *bookRT, prompt, mode string) error {
	if err := rt.eng.SetAdvanceMode(modeOf(mode)); err != nil {
		return err
	}
	s.db.touchBook(rt.id)
	go func() {
		if err := rt.eng.PrepareUserRules(prompt); err != nil {
			rt.hub.publishError("Chuẩn bị quy tắc thất bại: " + err.Error())
			return
		}
		if err := rt.eng.StartPrepared(prompt); err != nil {
			rt.hub.publishError("Không bắt đầu được: " + err.Error())
		}
	}()
	return nil
}

// handleStart 只用于还没有作品信息的书（例如上次启动裁定失败）。已有作品请用 resume。
func (s *server) handleStart(w http.ResponseWriter, r *http.Request, rt *bookRT) {
	var body struct {
		Prompt string `json:"prompt"`
		Mode   string `json:"mode"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	if book, _ := rt.store.Book.Load(); book != nil {
		writeErr(w, http.StatusConflict, fmt.Errorf("Truyện “%s” đã có nội dung, hãy dùng nút Chạy AI để tiếp tục", book.Title))
		return
	}
	prompt, err := startup.PrepareQuick(body.Prompt)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := s.startBook(rt, prompt, body.Mode); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true})
}

func (s *server) handleResume(w http.ResponseWriter, r *http.Request, rt *bookRT) {
	var body struct {
		Mode string `json:"mode"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	if body.Mode != "" {
		if err := rt.eng.SetAdvanceMode(modeOf(body.Mode)); err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
	}
	s.db.touchBook(rt.id)
	go func() {
		label, err := rt.eng.Resume()
		switch {
		case err != nil:
			rt.hub.publishError("Không tiếp tục được: " + err.Error())
		case label == "":
			rt.hub.publishError("Truyện này chưa có gì để tiếp tục, hãy nhập ý tưởng để bắt đầu")
		}
	}()
	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true})
}

func (s *server) handlePause(w http.ResponseWriter, r *http.Request, rt *bookRT) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": rt.eng.Abort()})
}

func (s *server) handleNext(w http.ResponseWriter, r *http.Request, rt *bookRT) {
	if err := rt.eng.AdvanceOneChapter(); err != nil {
		writeErr(w, http.StatusConflict, err)
		return
	}
	s.db.touchBook(rt.id)
	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true})
}

func (s *server) handleMode(w http.ResponseWriter, r *http.Request, rt *bookRT) {
	var body struct {
		Mode string `json:"mode"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	if err := rt.eng.SetAdvanceMode(modeOf(body.Mode)); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *server) handleSteer(w http.ResponseWriter, r *http.Request, rt *bookRT) {
	var body struct {
		Text      string `json:"text"`
		SessionID int64  `json:"session_id"`
		Chapter   int    `json:"chapter"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	text := strings.TrimSpace(body.Text)
	if text == "" {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("Nội dung trống"))
		return
	}
	sess, err := s.sessionFor(rt.id, body.SessionID, text)
	if err != nil {
		writeErr(w, statusOf(err), err)
		return
	}
	if _, err := s.db.addMessage(sess.ID, "steer", text, body.Chapter); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	go func() {
		if err := rt.eng.Steer(text); err != nil {
			rt.hub.publishError("Chỉ đạo thất bại: " + err.Error())
		}
	}()
	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true, "session": sess})
}

// ---- 工具 ----

func chapterParam(w http.ResponseWriter, r *http.Request) (int, bool) {
	n, err := strconv.Atoi(r.PathValue("n"))
	if err != nil || n < 1 || n > 9999 {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("số chương không hợp lệ"))
		return 0, false
	}
	return n, true
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 8<<20)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Warn("web: ghi JSON thất bại", "module", "web", "err", err)
	}
}

func writeErr(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]any{"error": err.Error()})
}

func statusOf(err error) int {
	if errors.Is(err, errNotFound) {
		return http.StatusNotFound
	}
	return http.StatusInternalServerError
}

func urlEscape(s string) string {
	var b strings.Builder
	for _, c := range []byte(s) {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.' {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// countWords 按空白切词：越南语以音节为词，与编辑器里的计数口径一致。
func countWords(s string) int {
	return len(strings.Fields(s))
}
