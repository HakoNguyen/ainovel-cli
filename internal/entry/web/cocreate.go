package web

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/voocel/ainovel-cli/internal/entry/startup"
	"github.com/voocel/ainovel-cli/internal/host"
)

// 共创（与 TUI 的 共创 模式同一套 Host 能力）：
//   - start：书还没有内容时，多轮对话澄清需求，产出整本书的创作指令；
//   - stage：书写到一半时暂停引擎，规划后续方向，结束时作为干预注入并继续创作。
//
// 会话落库，关掉页面再回来可以接着聊。
const (
	coStart = "start"
	coStage = "stage"
)

type coLine struct {
	Role string `json:"role"` // user / assistant
	Text string `json:"text"`
}

type coSession struct {
	Kind        string                 `json:"kind"`
	History     []host.CoCreateMessage `json:"-"` // 发给模型的完整历史（assistant 存四段原文）
	Display     []coLine               `json:"display"`
	Draft       string                 `json:"draft"`
	Ready       bool                   `json:"ready"`
	Suggestions []string               `json:"suggestions"`
	DraftEdited bool                   `json:"draft_edited"` // 用户在上次回复后手动改过草稿
	UpdatedAt   string                 `json:"updated_at"`
}

// ---- 存储 ----

func (d *db) getCoCreate(bookID, kind string) (*coSession, error) {
	var hist, disp, sugg string
	s := &coSession{Kind: kind}
	err := d.sql.QueryRow(`SELECT history, display, draft, ready, suggestions, draft_edited, updated_at
		FROM cocreate WHERE book_id = ? AND kind = ?`, bookID, kind).
		Scan(&hist, &disp, &s.Draft, &s.Ready, &sugg, &s.DraftEdited, &s.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(hist), &s.History)
	_ = json.Unmarshal([]byte(disp), &s.Display)
	_ = json.Unmarshal([]byte(sugg), &s.Suggestions)
	return s, nil
}

func (d *db) saveCoCreate(bookID string, s *coSession) error {
	hist, _ := json.Marshal(s.History)
	disp, _ := json.Marshal(s.Display)
	if s.Suggestions == nil {
		s.Suggestions = []string{}
	}
	sugg, _ := json.Marshal(s.Suggestions)
	s.UpdatedAt = now()
	_, err := d.sql.Exec(`INSERT INTO cocreate (book_id, kind, history, display, draft, ready, suggestions, draft_edited, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(book_id, kind) DO UPDATE SET history = excluded.history, display = excluded.display,
			draft = excluded.draft, ready = excluded.ready, suggestions = excluded.suggestions,
			draft_edited = excluded.draft_edited, updated_at = excluded.updated_at`,
		bookID, s.Kind, string(hist), string(disp), s.Draft, s.Ready, string(sugg), s.DraftEdited, s.UpdatedAt)
	return err
}

func (d *db) deleteCoCreate(bookID, kind string) error {
	_, err := d.sql.Exec(`DELETE FROM cocreate WHERE book_id = ? AND kind = ?`, bookID, kind)
	return err
}

// ---- 处理 ----

var coBusy sync.Map // bookID/kind -> struct{}：同一会话同时只跑一轮

func coKind(k string) (string, error) {
	switch k {
	case coStart, coStage:
		return k, nil
	}
	return "", fmt.Errorf("loại phiên không hợp lệ: %q", k)
}

// coAllowed 检查这本书当前能否进行该类共创。
func coAllowed(rt *bookRT, kind string) error {
	book, _ := rt.store.Book.Load()
	snap := rt.eng.Snapshot()
	switch kind {
	case coStart:
		if book != nil {
			return fmt.Errorf("Truyện đã có nội dung, hãy dùng “Bàn hướng đi” để định hướng phần tiếp theo")
		}
		if snap.IsRunning {
			return fmt.Errorf("AI đang chạy truyện này")
		}
	case coStage:
		if book == nil {
			return fmt.Errorf("Truyện chưa có nội dung, hãy xây dựng ý tưởng trước")
		}
		if snap.Phase == "complete" {
			return fmt.Errorf("Truyện đã hoàn tất")
		}
	}
	return nil
}

func (s *server) handleGetCoCreate(w http.ResponseWriter, r *http.Request, rt *bookRT) {
	kind, err := coKind(r.URL.Query().Get("kind"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	sess, err := s.db.getCoCreate(rt.id, kind)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"session": sess})
}

func (s *server) handleCoCreateMessage(w http.ResponseWriter, r *http.Request, rt *bookRT) {
	var body struct {
		Kind string `json:"kind"`
		Text string `json:"text"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	kind, err := coKind(body.Kind)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	text := strings.TrimSpace(body.Text)
	if text == "" {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("Chưa nhập nội dung"))
		return
	}
	if err := coAllowed(rt, kind); err != nil {
		writeErr(w, http.StatusConflict, err)
		return
	}
	key := rt.id + "/" + kind
	if _, busy := coBusy.LoadOrStore(key, struct{}{}); busy {
		writeErr(w, http.StatusConflict, fmt.Errorf("AI đang trả lời lượt trước, hãy đợi một chút"))
		return
	}
	defer coBusy.Delete(key)

	sess, err := s.db.getCoCreate(rt.id, kind)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if sess == nil {
		sess = &coSession{Kind: kind}
		if kind == coStage {
			// 进入阶段共创：引擎运行中则暂停；已在共创中时返回 false，无妨。
			rt.eng.PauseForCoCreate()
		}
	}

	// 用户手动改过草稿时，把改后的版本带给模型，免得它在旧草稿上继续累积。
	modelText := text
	if sess.DraftEdited && strings.TrimSpace(sess.Draft) != "" {
		modelText = "(Tôi đã tự chỉnh bản yêu cầu sáng tác như dưới đây, hãy lấy bản này làm gốc:)\n" +
			sess.Draft + "\n\n" + text
	}
	history := append(append([]host.CoCreateMessage(nil), sess.History...), host.CoCreateMessage{Role: "user", Content: modelText})

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, fmt.Errorf("streaming unsupported"))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	send := func(v any) {
		payload, _ := json.Marshal(v)
		fmt.Fprintf(w, "data: %s\n\n", payload)
		flusher.Flush()
	}

	stream := rt.eng.CoCreateStream
	if kind == coStage {
		stream = rt.eng.StageCoCreateStream
	}
	reply, err := stream(r.Context(), history, func(progress, text string) {
		send(map[string]string{"type": progress, "text": text})
	})
	if err != nil {
		send(map[string]string{"type": "error", "text": err.Error()})
		return
	}

	raw := strings.TrimSpace(reply.Raw)
	if raw == "" {
		raw = strings.TrimSpace(reply.Message)
	}
	sess.History = append(history, host.CoCreateMessage{Role: "assistant", Content: raw})
	sess.Display = append(sess.Display, coLine{Role: "user", Text: text}, coLine{Role: "assistant", Text: strings.TrimSpace(reply.Message)})
	if p := strings.TrimSpace(reply.Prompt); p != "" {
		sess.Draft = p
	}
	sess.Ready = reply.Ready
	sess.Suggestions = append([]string{}, reply.Suggestions...)
	sess.DraftEdited = false
	if err := s.db.saveCoCreate(rt.id, sess); err != nil {
		send(map[string]string{"type": "error", "text": "Lưu phiên thất bại: " + err.Error()})
		return
	}
	s.db.touchBook(rt.id)
	send(map[string]any{"type": "result", "session": sess})
}

// handleCoCreateDraft 保存用户手动编辑的草稿。
func (s *server) handleCoCreateDraft(w http.ResponseWriter, r *http.Request, rt *bookRT) {
	var body struct {
		Kind  string `json:"kind"`
		Draft string `json:"draft"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	kind, err := coKind(body.Kind)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	sess, err := s.db.getCoCreate(rt.id, kind)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if sess == nil {
		sess = &coSession{Kind: kind}
	}
	if sess.Draft != body.Draft {
		sess.Draft = body.Draft
		sess.DraftEdited = true
	}
	if err := s.db.saveCoCreate(rt.id, sess); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleCoCreateApply 用草稿开始创作（start）或注入后续方向并继续（stage）。
func (s *server) handleCoCreateApply(w http.ResponseWriter, r *http.Request, rt *bookRT) {
	var body struct {
		Kind  string `json:"kind"`
		Mode  string `json:"mode"`
		Draft string `json:"draft"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	kind, err := coKind(body.Kind)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	draft := strings.TrimSpace(body.Draft)
	if draft == "" {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("Bản yêu cầu đang trống, hãy trò chuyện với AI thêm một chút"))
		return
	}
	if err := coAllowed(rt, kind); err != nil {
		writeErr(w, http.StatusConflict, err)
		return
	}
	if refuseWhileStarting(w, rt) {
		return
	}

	switch kind {
	case coStart:
		prompt, err := startup.PrepareQuick(draft)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		if err := s.startBook(rt, prompt, body.Mode); err != nil {
			writeErr(w, statusOf(err), err)
			return
		}
	case coStage:
		if body.Mode != "" {
			if err := rt.eng.SetAdvanceMode(modeOf(body.Mode)); err != nil {
				writeErr(w, http.StatusInternalServerError, err)
				return
			}
		}
		// 共创占用标记可能因重开 Host 丢失：先补上（已在共创中会返回 false），再注入方向。
		rt.eng.PauseForCoCreate()
		go func() {
			if err := rt.eng.ResumeFromCoCreate(draft); err != nil {
				rt.hub.publishError("Áp dụng hướng đi thất bại: " + err.Error())
			}
		}()
	}
	if err := s.db.deleteCoCreate(rt.id, kind); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true})
}

// handleCoCreateCancel 放弃会话。stage 会让引擎保持暂停，start 只清掉对话。
func (s *server) handleCoCreateCancel(w http.ResponseWriter, r *http.Request, rt *bookRT) {
	var body struct {
		Kind string `json:"kind"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	kind, err := coKind(body.Kind)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if kind == coStage {
		rt.eng.CancelCoCreate()
	}
	if err := s.db.deleteCoCreate(rt.id, kind); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
