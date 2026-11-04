package web

import (
	"encoding/json"
	"testing"
	"time"
)

func recvMsg(t *testing.T, ch chan []byte) (string, any) {
	t.Helper()
	select {
	case raw := <-ch:
		var m struct {
			Type string `json:"type"`
			Data any    `json:"data"`
		}
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatal(err)
		}
		return m.Type, m.Data
	case <-time.After(2 * time.Second):
		t.Fatal("không nhận được tin")
		return "", nil
	}
}

func TestHubCoalescesDeltasInOrder(t *testing.T) {
	h := newHub()
	ch := make(chan []byte, 16)
	h.clients[ch] = struct{}{}

	h.publishDelta("Mùi ")
	h.publishDelta("thuốc ")
	h.publishDelta("và máu")
	// Sự kiện tới trước hạn gộp: phần chữ đang chờ phải được đẩy đi trước nó.
	h.publishEvent(eventPayload{Category: "TOOL", Summary: "save_book"})

	if typ, data := recvMsg(t, ch); typ != "delta" || data != "Mùi thuốc và máu" {
		t.Fatalf("muốn một delta gộp, nhận %s %v", typ, data)
	}
	if typ, _ := recvMsg(t, ch); typ != "event" {
		t.Fatalf("muốn event sau delta, nhận %s", typ)
	}

	h.publishDelta("tiếp")
	if typ, data := recvMsg(t, ch); typ != "delta" || data != "tiếp" {
		t.Fatalf("delta lẻ phải tự đẩy sau thời gian gộp, nhận %s %v", typ, data)
	}
	select {
	case raw := <-ch:
		t.Fatalf("không được gửi trùng: %s", raw)
	case <-time.After(deltaFlushDelay * 2):
	}
}
