package host

import (
	"context"
	"fmt"
	"strings"

	"github.com/voocel/agentcore"
)

// AssistStream 是网页编辑器侧栏的自由对话：不进入引擎、不改动作品事实，
// 只把模型回复逐段交给 onDelta。用量计入 "assist" 角色，方便在用量面板里区分。
func (h *Host) AssistStream(ctx context.Context, system string, history []CoCreateMessage, onDelta func(string)) error {
	msgs := []agentcore.Message{agentcore.SystemMsg(system)}
	for _, item := range history {
		content := strings.TrimSpace(item.Content)
		if content == "" {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(item.Role), "assistant") {
			msgs = append(msgs, assistantMsg(content))
		} else {
			msgs = append(msgs, agentcore.UserMsg(content))
		}
	}
	if len(msgs) == 1 {
		return fmt.Errorf("assist history is empty")
	}

	model := newUsageTrackedModel(h.models.ForRole("writer"), "assist", h.usage.Record)
	streamCh, err := model.GenerateStream(ctx, msgs, nil, agentcore.WithMaxTokens(4096))
	if err != nil {
		return fmt.Errorf("assist generate: %w", err)
	}
	var streamed bool
	for ev := range streamCh {
		switch ev.Type {
		case agentcore.StreamEventTextDelta:
			streamed = true
			onDelta(ev.Delta)
		case agentcore.StreamEventDone:
			if !streamed {
				onDelta(ev.Message.TextContent())
			}
		case agentcore.StreamEventError:
			if ev.Err != nil {
				return fmt.Errorf("assist generate: %w", ev.Err)
			}
			return fmt.Errorf("assist generate failed")
		}
	}
	return nil
}
