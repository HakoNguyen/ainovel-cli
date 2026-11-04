package web

import (
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/voocel/ainovel-cli/internal/store"
)

// displayName 决定书库里显示的书名：用户手动改过的名字优先，否则用 AI 在作品里定的书名，
// 都没有时才用登记时的临时名。只影响书库与助手的称呼，不改动作品文件里的书名。
func displayName(row bookRow, aiTitle string) string {
	if row.Renamed || strings.TrimSpace(aiTitle) == "" {
		return row.Name
	}
	return aiTitle
}

func bookTitleAt(dir string) string {
	if book, _ := store.NewStore(dir).Book.Load(); book != nil {
		return book.Title
	}
	return ""
}

// backfillRenamed 在刚补上 renamed 列时运行一次：旧版本里书库名与作品书名不同，
// 只可能是用户手动改过名，按"已改名"处理，避免升级后名字被 AI 书名盖掉。
func backfillRenamed(d *db) {
	if !d.addedRenamed {
		return
	}
	for _, archived := range []bool{false, true} {
		rows, err := d.listBooks(archived)
		if err != nil {
			slog.Warn("web: đọc danh sách truyện để nâng cấp thất bại", "module", "web", "err", err)
			return
		}
		for _, row := range rows {
			if title := bookTitleAt(row.Dir); title != "" && title != row.Name {
				_ = d.markRenamed(row.ID)
			}
		}
	}
}

type diskBook struct {
	Dir      string `json:"dir"`
	Title    string `json:"title"`
	Chapters int    `json:"chapters"`
}

// scanDisk 在工作目录下（最多三层）找出还没登记进书库的作品目录：
// 有 meta/book.json 或 premise.md 的目录即视为一本书。
func (s *server) scanDisk() ([]diskBook, error) {
	known, err := s.db.knownDirs()
	if err != nil {
		return nil, err
	}
	out := []diskBook{}
	err = filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return nil
		}
		name := d.Name()
		if path != "." && (strings.HasPrefix(name, ".") || name == "logs" || name == userDraftDir ||
			name == "meta" || name == "chapters" || name == "drafts" || name == "summaries" || name == "reviews") {
			return filepath.SkipDir
		}
		if strings.Count(path, string(filepath.Separator)) >= 3 {
			return filepath.SkipDir
		}
		if path == "." || known[filepath.Clean(path)] {
			return nil
		}
		if !isBookDir(path) {
			return nil
		}
		st := store.NewStore(path)
		item := diskBook{Dir: filepath.Clean(path), Title: bookTitleAt(path)}
		if p, _ := st.Progress.Load(); p != nil {
			item.Chapters = len(p.CompletedChapters)
		}
		out = append(out, item)
		return filepath.SkipDir
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Dir < out[j].Dir })
	return out, err
}

func isBookDir(dir string) bool {
	for _, marker := range []string{filepath.Join("meta", "book.json"), "premise.md"} {
		if _, err := os.Stat(filepath.Join(dir, marker)); err == nil {
			return true
		}
	}
	return false
}

func (s *server) handleScanDisk(w http.ResponseWriter, r *http.Request) {
	found, err := s.scanDisk()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, found)
}

// handleImportDir 把扫描出来的目录登记进书库。只接受扫描结果里的目录，不接受任意路径。
func (s *server) handleImportDir(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Dir string `json:"dir"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	found, err := s.scanDisk()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	want := filepath.Clean(body.Dir)
	for _, item := range found {
		if item.Dir != want {
			continue
		}
		name := item.Title
		if name == "" {
			name = filepath.Base(item.Dir)
		}
		row, err := s.db.addBook(newID(), name, item.Dir)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusCreated, row)
		return
	}
	writeErr(w, http.StatusNotFound, fmt.Errorf("Không thấy thư mục truyện %q (hoặc nó đã có trong thư viện)", body.Dir))
}

// safeBookDir 确认目录位于工作目录之内、且确实是一本书的目录，才允许整个删除。
func safeBookDir(dir string) (string, error) {
	clean := filepath.Clean(dir)
	if clean == "." {
		return "", fmt.Errorf("không xoá thư mục gốc workspace")
	}
	if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("thư mục %q nằm ngoài workspace, không xoá", dir)
	}
	root, err := filepath.Abs(".")
	if err != nil {
		return "", err
	}
	abs, err := filepath.Abs(clean)
	if err != nil {
		return "", err
	}
	if rel, err := filepath.Rel(root, abs); err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return "", fmt.Errorf("thư mục %q nằm ngoài workspace, không xoá", dir)
	}
	return abs, nil
}

// handlePurgeBook 永久删除：书库记录、对话、共创会话，以及整个作品目录（含作者稿与图片）。
// 不可恢复，前端需二次确认；运行中的书拒绝删除。
func (s *server) handlePurgeBook(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	row, err := s.db.getBookAny(id)
	if err != nil {
		writeErr(w, statusOf(err), err)
		return
	}
	if rt := s.lib.peek(id); rt != nil && rt.eng.Snapshot().IsRunning {
		writeErr(w, http.StatusConflict, fmt.Errorf("AI đang chạy truyện này, hãy dừng trước"))
		return
	}
	abs, err := safeBookDir(row.Dir)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	// 目录存在时必须像一本书，避免误删登记错的普通目录；目录已不在则只清记录。
	_, statErr := os.Stat(abs)
	exists := statErr == nil
	if exists && !isBookDir(abs) && !isEmptyDir(abs) {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("thư mục %q không giống thư mục truyện, không xoá", row.Dir))
		return
	}
	s.lib.closeBook(id) // 先释放目录租约
	if err := s.db.purgeBook(id); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if exists {
		if err := os.RemoveAll(abs); err != nil {
			writeErr(w, http.StatusInternalServerError, fmt.Errorf("đã xoá khỏi thư viện nhưng xoá thư mục thất bại: %w", err))
			return
		}
	}
	slog.Info("web: đã xoá vĩnh viễn truyện", "module", "web", "book", id, "dir", row.Dir)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func isEmptyDir(dir string) bool {
	entries, err := os.ReadDir(dir)
	return err == nil && len(entries) == 0
}

func (s *server) handleRestoreBook(w http.ResponseWriter, r *http.Request) {
	if err := s.db.restoreBook(r.PathValue("id")); err != nil {
		writeErr(w, statusOf(err), err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
