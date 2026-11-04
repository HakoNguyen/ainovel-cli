package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/voocel/ainovel-cli/internal/host"
)

const assistSystemPrompt = `Bạn là trợ lý sáng tác ngồi cạnh một tác giả đang TỰ TAY viết tiểu thuyết. Bên trái màn hình là dàn ý và bản AI tham khảo, bên phải là bản thảo của chính tác giả.

Nguyên tắc:
- Tác giả là người viết. Bạn gợi ý, phản biện, đặt câu hỏi, đưa phương án — chỉ viết hẳn văn xuôi khi được yêu cầu rõ ràng (ví dụ "viết tiếp", "viết lại đoạn này"), và khi đó viết ngắn gọn, đúng giọng văn hiện có của tác giả.
- Bám sát dàn ý, nhân vật và bối cảnh đã có; nếu tác giả đi lệch dàn ý, chỉ ra chỗ lệch và hỏi họ có muốn giữ không, đừng tự sửa.
- Nhận xét cụ thể: trích câu, chỉ chỗ, nói vì sao. Tránh khen chung chung.
- Tránh văn AI sáo rỗng: không ẩn dụ chồng chất, không kết đoạn bằng câu triết lý, không lặp cấu trúc "không phải X mà là Y".
- Trả lời bằng %s, gọn, dùng Markdown nhẹ (gạch đầu dòng, in đậm) khi cần.

Dưới đây là ngữ cảnh hiện tại của tác phẩm.
`

type chatRequest struct {
	SessionID int64  `json:"session_id"`
	Text      string `json:"text"`
	Chapter   int    `json:"chapter"`
	Mine      string `json:"mine"`
	Selection string `json:"selection"`
}

// handleChat 把用户消息落库后流式返回助手回复，回复结束（或中断）时也落库。
// 历史由服务端从数据库读取，浏览器只发本轮输入。
func (s *server) handleChat(w http.ResponseWriter, r *http.Request, rt *bookRT) {
	var req chatRequest
	if !readJSON(w, r, &req) {
		return
	}
	text := strings.TrimSpace(req.Text)
	if text == "" {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("Chưa nhập nội dung"))
		return
	}
	sess, err := s.sessionFor(rt.id, req.SessionID, text)
	if err != nil {
		writeErr(w, statusOf(err), err)
		return
	}
	stored, err := s.db.listMessages(sess.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if _, err := s.db.addMessage(sess.ID, "user", text, req.Chapter); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	s.db.touchBook(rt.id)

	var history []host.CoCreateMessage
	for _, m := range stored {
		if m.Role == "user" || m.Role == "assistant" {
			history = append(history, host.CoCreateMessage{Role: m.Role, Content: m.Content})
		}
	}
	if len(history) > 40 {
		history = history[len(history)-40:]
	}
	history = append(history, host.CoCreateMessage{Role: "user", Content: text})

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
	send(map[string]any{"type": "session", "session": sess})

	var answer strings.Builder
	err = rt.eng.AssistStream(r.Context(), s.assistContext(rt, req), history, func(delta string) {
		answer.WriteString(delta)
		send(map[string]string{"type": "delta", "text": delta})
	})
	if reply := strings.TrimSpace(answer.String()); reply != "" {
		// 请求被取消时 r.Context 已失效，但数据库写入不依赖它，半截回复也保留。
		if _, dbErr := s.db.addMessage(sess.ID, "assistant", reply, req.Chapter); dbErr != nil {
			send(map[string]string{"type": "error", "text": "Lưu câu trả lời thất bại: " + dbErr.Error()})
		}
	}
	if err != nil {
		send(map[string]string{"type": "error", "text": err.Error()})
		return
	}
	send(map[string]string{"type": "done"})
}

// sessionFor 取出属于该书的会话；id 为 0 时以首条消息为标题新建一个。
func (s *server) sessionFor(bookID string, id int64, firstText string) (sessionRow, error) {
	if id == 0 {
		return s.db.createSession(bookID, clip(strings.Join(strings.Fields(firstText), " "), 48))
	}
	sess, err := s.db.getSession(id)
	if err != nil {
		return sess, err
	}
	if sess.BookID != bookID {
		return sess, errNotFound
	}
	return sess, nil
}

// assistContext 拼出给助手的作品上下文。各段都有长度上限，避免小模型上下文溢出。
func (s *server) assistContext(rt *bookRT, req chatRequest) string {
	lang := "tiếng Việt"
	if s.cfg.NormalizedLanguage() == "zh" {
		lang = "tiếng Trung (中文)"
	}
	var b strings.Builder
	b.WriteString(fmt.Sprintf(assistSystemPrompt, lang))

	if book, _ := rt.store.Book.Load(); book != nil {
		name := book.Title
		if row, err := s.db.getBook(rt.id); err == nil {
			name = displayName(row, book.Title)
		}
		section(&b, "Tên truyện", name)
		section(&b, "Tóm tắt", clip(book.Synopsis, 1500))
	}
	if premise, _ := rt.store.Outline.LoadPremise(); premise != "" {
		section(&b, "Tiền đề", clip(premise, 2500))
	}
	if chars, _ := rt.store.Characters.Load(); len(chars) > 0 {
		var cb strings.Builder
		for i, c := range chars {
			if i >= 15 {
				break
			}
			fmt.Fprintf(&cb, "- %s (%s): %s\n", c.Name, c.Role, clip(c.Description, 200))
		}
		section(&b, "Nhân vật chính", cb.String())
	}

	if n := req.Chapter; n > 0 {
		if e := outlineEntry(rt.store, n); e != nil {
			var ob strings.Builder
			fmt.Fprintf(&ob, "Chương %d: %s\nSự kiện chính: %s\n", n, e.Title, e.CoreEvent)
			if e.Hook != "" {
				fmt.Fprintf(&ob, "Móc cuối chương: %s\n", e.Hook)
			}
			for _, sc := range e.Scenes {
				fmt.Fprintf(&ob, "- Cảnh: %s\n", sc)
			}
			section(&b, "Dàn ý chương đang viết", ob.String())
		}
		if prev := strings.TrimSpace(loadMine(rt.store.Dir(), n-1)); prev != "" {
			section(&b, fmt.Sprintf("Đoạn cuối chương %d của tác giả (để nối mạch)", n-1), tail(prev, 1200))
		}
		if ai, _ := rt.store.Drafts.LoadChapterText(n); ai != "" {
			section(&b, "Bản AI tham khảo của chương này (trích)", clip(ai, 3000))
		}
	}
	if mine := strings.TrimSpace(req.Mine); mine != "" {
		section(&b, "Bản thảo hiện tại của tác giả (phần cuối)", tail(mine, 6000))
	} else {
		section(&b, "Bản thảo hiện tại của tác giả", "(chưa viết gì)")
	}
	if sel := strings.TrimSpace(req.Selection); sel != "" {
		section(&b, "Đoạn tác giả đang bôi đen", clip(sel, 3000))
	}
	return b.String()
}

func section(b *strings.Builder, title, body string) {
	body = strings.TrimSpace(body)
	if body == "" {
		return
	}
	b.WriteString("\n## " + title + "\n" + body + "\n")
}

func clip(s string, maxRunes int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) <= maxRunes {
		return string(r)
	}
	return string(r[:maxRunes]) + "…"
}

func tail(s string, maxRunes int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) <= maxRunes {
		return string(r)
	}
	return "…" + string(r[len(r)-maxRunes:])
}
