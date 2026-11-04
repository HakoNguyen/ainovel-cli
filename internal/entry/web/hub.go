package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	recentEventCap = 300
	liveTextCap    = 200_000 // 单轮流式缓存上限（字节），超出只保留尾部
)

type eventPayload struct {
	Time     string `json:"time"`
	Category string `json:"category"`
	Summary  string `json:"summary"`
	Detail   string `json:"detail,omitempty"`
	Level    string `json:"level,omitempty"`
}

// hub 把同一份事件扇出给所有浏览器标签页，并保留最近事件和本轮流式文本，
// 让刷新页面后能立刻看到正在发生的事。
type hub struct {
	mu      sync.Mutex
	clients map[chan []byte]struct{}
	recent  [][]byte
	live    strings.Builder
}

func newHub() *hub {
	return &hub{clients: map[chan []byte]struct{}{}}
}

func encodeMsg(kind string, data any) []byte {
	payload, _ := json.Marshal(map[string]any{"type": kind, "data": data})
	return payload
}

func (h *hub) broadcastLocked(msg []byte) {
	for ch := range h.clients {
		select {
		case ch <- msg:
		default: // 慢客户端丢弃本条，不阻塞引擎
		}
	}
}

func (h *hub) publish(kind string, data any) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.broadcastLocked(encodeMsg(kind, data))
}

func (h *hub) publishEvent(ev eventPayload) {
	msg := encodeMsg("event", ev)
	h.mu.Lock()
	defer h.mu.Unlock()
	h.recent = append(h.recent, msg)
	if len(h.recent) > recentEventCap {
		h.recent = h.recent[len(h.recent)-recentEventCap:]
	}
	h.broadcastLocked(msg)
}

func (h *hub) publishError(summary string) {
	h.publishEvent(eventPayload{Time: time.Now().Format("15:04:05"), Category: "ERROR", Summary: summary, Level: "error"})
}

func (h *hub) publishDelta(delta string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.live.WriteString(delta)
	if h.live.Len() > liveTextCap {
		s := h.live.String()
		i := len(s) - liveTextCap/2
		for i < len(s) && !utf8.RuneStart(s[i]) {
			i++
		}
		h.live.Reset()
		h.live.WriteString(s[i:])
	}
	h.broadcastLocked(encodeMsg("delta", delta))
}

func (h *hub) publishClear() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.live.Reset()
	h.broadcastLocked(encodeMsg("clear", nil))
}

func (s *server) handleEvents(w http.ResponseWriter, r *http.Request, rt *bookRT) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ch := make(chan []byte, 512)
	h := rt.hub
	h.mu.Lock()
	backlog := append([][]byte(nil), h.recent...)
	backlog = append(backlog, encodeMsg("live", h.live.String()))
	h.clients[ch] = struct{}{}
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		delete(h.clients, ch)
		h.mu.Unlock()
	}()

	for _, msg := range backlog {
		fmt.Fprintf(w, "data: %s\n\n", msg)
	}
	flusher.Flush()

	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case msg := <-ch:
			fmt.Fprintf(w, "data: %s\n\n", msg)
			flusher.Flush()
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		}
	}
}
