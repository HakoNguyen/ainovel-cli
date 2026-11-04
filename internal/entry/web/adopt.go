package web

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/voocel/ainovel-cli/internal/domain"
)

// 采用作者稿：把作者自己写的某章换成正式章节，再走引擎的修订同步（与 TUI /sync 同一条路径）。
// 同步会用作者稿重新抽取该章事实（摘要、人物、时间线、伏笔…），记录 origin=user 并学习作者文风，
// 之后 AI 续写的章节就接在作者稿后面。只适用于 AI 已提交过的章节——修订机制以已接纳记录为基线。

const aiOriginalDir = "ai_original" // 采用前的 AI 原稿备份，供"Bản AI viết"页继续展示

func aiOriginalPath(dir string, n int) string {
	return filepath.Join(dir, userDraftDir, aiOriginalDir, fmt.Sprintf("%02d.md", n))
}

// chapterContentFromMine 把编辑器导出的纯文本整理成章节正文：段落之间空一行。
// 作者稿没写章节标题时沿用 AI 原稿的 Markdown 标题行，保持各章标题格式一致。
func chapterContentFromMine(mine, aiOriginal string) string {
	var paras []string
	for _, line := range strings.Split(domain.NormalizeChapterContent(mine), "\n") {
		if t := strings.TrimSpace(line); t != "" {
			paras = append(paras, t)
		}
	}
	if len(paras) == 0 {
		return ""
	}
	heading := ""
	for _, line := range strings.Split(domain.NormalizeChapterContent(aiOriginal), "\n") {
		if t := strings.TrimSpace(line); t != "" {
			if strings.HasPrefix(t, "#") {
				heading = t
			}
			break
		}
	}
	if heading != "" && !strings.HasPrefix(paras[0], "#") {
		bare := strings.TrimSpace(strings.TrimLeft(heading, "#"))
		if strings.EqualFold(paras[0], bare) {
			paras[0] = heading
		} else {
			paras = append([]string{heading}, paras...)
		}
	}
	return strings.Join(paras, "\n\n") + "\n"
}

func chapterCompleted(rt *bookRT, n int) bool {
	p, _ := rt.store.Progress.Load()
	return p != nil && slices.Contains(p.CompletedChapters, n)
}

// adoptState 返回该章正式稿是否已经是作者稿当前版本，以及修订同步是否还欠着。
func adoptState(rt *bookRT, n int) (mineIsOfficial bool) {
	official, _ := rt.store.Drafts.LoadChapterText(n)
	backup, _ := os.ReadFile(aiOriginalPath(rt.store.Dir(), n))
	want := chapterContentFromMine(loadMine(rt.store.Dir(), n), string(backup))
	return want != "" && domain.NormalizeChapterContent(official) == want
}

func (s *server) handleAdoptMine(w http.ResponseWriter, r *http.Request, rt *bookRT) {
	n, ok := chapterParam(w, r)
	if !ok {
		return
	}
	if refuseWhileStarting(w, rt) {
		return
	}
	if rt.eng.Snapshot().IsRunning {
		writeErr(w, http.StatusConflict, fmt.Errorf("AI đang chạy, hãy dừng trước khi thay chương"))
		return
	}
	if !chapterCompleted(rt, n) {
		writeErr(w, http.StatusConflict, fmt.Errorf("AI chưa chốt chương %d. Bấm “AI viết chương kế” để AI viết chương này trước, rồi mới thay bằng bản của bạn", n))
		return
	}
	dir := rt.store.Dir()
	mine := loadMine(dir, n)
	if countWords(mine) < 50 {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("Bản của bạn cho chương %d còn quá ngắn (%d chữ)", n, countWords(mine)))
		return
	}

	// 备份 AI 原稿：只在第一次采用时写，之后的修订都以作者稿为准。
	backupPath := aiOriginalPath(dir, n)
	if _, err := os.Stat(backupPath); os.IsNotExist(err) {
		original, err := rt.store.Drafts.LoadChapterText(n)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		if err := writeAtomic(backupPath, []byte(original)); err != nil {
			writeErr(w, http.StatusInternalServerError, fmt.Errorf("sao lưu bản AI thất bại: %w", err))
			return
		}
	}
	backup, _ := os.ReadFile(backupPath)
	content := chapterContentFromMine(mine, string(backup))
	if err := rt.store.Drafts.SaveFinalChapter(n, content); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}

	// 同步要调一次模型；浏览器断开也让它跑完，否则会留下"正文已改但未同步"的半状态。
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 10*time.Minute)
	defer cancel()
	res, err := rt.eng.SyncChapterRevisions(ctx)
	if err != nil {
		writeErr(w, http.StatusBadGateway, fmt.Errorf("Đã ghi bản của bạn nhưng engine chưa đồng bộ xong: %v. Bấm lại nút này để thử lại", err))
		return
	}
	s.db.touchBook(rt.id)
	rt.hub.publishEvent(eventPayload{Time: time.Now().Format("15:04:05"), Category: "SYSTEM", Level: "info",
		Summary: fmt.Sprintf("Đã dùng bản của bạn làm chương %d chính thức; các chương AI viết sau sẽ nối tiếp bản này", n)})

	var summary string
	var issues, style []string
	for _, a := range res.Analyses {
		if a.ChangeSummary != "" {
			summary = a.ChangeSummary
		}
		issues = append(issues, a.DownstreamIssues...)
		style = append(style, a.StyleDelta.Prose...)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "applied": res.Applied,
		"change_summary": summary, "downstream_issues": issues, "style_learned": style,
	})
}
