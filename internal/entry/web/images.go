package web

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
)

const maxImageBytes = 10 << 20

// 编辑器插入的图片存到作者稿目录下，HTML 里只保留相对链接，避免 base64 把章节文件撑大。
var (
	imageExts = map[string]string{"image/png": ".png", "image/jpeg": ".jpg", "image/gif": ".gif", "image/webp": ".webp"}
	imageName = regexp.MustCompile(`^[0-9a-f]{16}\.(png|jpg|gif|webp)$`)
)

func imageDir(bookDir string) string {
	return filepath.Join(bookDir, userDraftDir, "images")
}

func (s *server) handleUploadImage(w http.ResponseWriter, r *http.Request) {
	row, err := s.db.getBook(r.PathValue("id"))
	if err != nil {
		writeErr(w, statusOf(err), err)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxImageBytes+1024)
	file, _, err := r.FormFile("file")
	if err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("Ảnh không hợp lệ hoặc lớn hơn 10MB"))
		return
	}
	defer file.Close()
	data, err := io.ReadAll(file)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	ext, ok := imageExts[http.DetectContentType(data)]
	if !ok {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("Chỉ hỗ trợ ảnh PNG, JPEG, GIF, WebP"))
		return
	}
	name := newID() + newID() + ext
	if err := writeAtomic(filepath.Join(imageDir(row.Dir), name), data); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"url": fmt.Sprintf("api/books/%s/images/%s", row.ID, name)})
}

func (s *server) handleGetImage(w http.ResponseWriter, r *http.Request) {
	row, err := s.db.getBook(r.PathValue("id"))
	if err != nil {
		writeErr(w, statusOf(err), err)
		return
	}
	name := r.PathValue("name")
	if !imageName.MatchString(name) {
		http.NotFound(w, r)
		return
	}
	path := filepath.Join(imageDir(row.Dir), name)
	if _, err := os.Stat(path); err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
	http.ServeFile(w, r, path)
}
