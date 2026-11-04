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
	liveTextCap    = 80_000 // 单轮流式缓存上限（字节），超出只保留后一半
	// 模型每秒吐出几十个小片段；攒一小段再推给浏览器，消息数降一个数量级，
	// 浏览器也不必每个 token 都重排一次版面。
	deltaFlushDelay = 150 * time.Millisecond
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
	pending strings.Builder // 尚未推送的流式片段
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

// flushLocked 推送攒下的流式片段。其他消息发出前都先调用它，保证浏览器看到的顺序不乱。
func (h *hub) flushLocked() {
	if h.pending.Len() == 0 {
		return
	}
	h.broadcastLocked(encodeMsg("delta", h.pending.String()))
	h.pending.Reset()
}

func (h *hub) publish(kind string, data any) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.flushLocked()
	h.broadcastLocked(encodeMsg(kind, data))
}

func (h *hub) publishEvent(ev eventPayload) {
	msg := encodeMsg("event", ev)
	h.mu.Lock()
	defer h.mu.Unlock()
	h.flushLocked()
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
	if h.pending.Len() == 0 {
		time.AfterFunc(deltaFlushDelay, func() {
			h.mu.Lock()
			h.flushLocked()
			h.mu.Unlock()
		})
	}
	h.pending.WriteString(delta)
}

func (h *hub) publishClear() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.flushLocked()
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
	h.flushLocked() // 攒着的片段已写进 live，先推给旧连接，免得新连接再收到一遍
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
