package facade

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/oai-prism/oaiprism/internal/sse"
)

// Progress items retain their identity across cumulative preview extensions.
// Their completed forms are also used verbatim in response.completed.
type responsesProgress struct {
	writer     *sse.Writer
	startIndex int
	items      []json.RawMessage
	states     []*responsesProgressItem
	keys       map[string]int
}

type responsesProgressItem struct {
	id        string
	index     int
	reasoning bool
	text      strings.Builder
}

func (p *responsesProgress) emit(d Delta) error {
	for _, ev := range d.Progress {
		key := ev.Type + ":" + strconv.Itoa(ev.LineIndex)
		if err := p.delta(key, ev.Type == "agent_reasoning", ev.Text); err != nil {
			return err
		}
	}
	if d.Reasoning != "" {
		return p.delta("output_reasoning", true, d.Reasoning)
	}
	return nil
}

func (p *responsesProgress) delta(key string, reasoning bool, text string) error {
	if text == "" {
		return nil
	}
	if p.keys == nil {
		p.keys = make(map[string]int)
	}
	i, exists := p.keys[key]
	if !exists {
		i = len(p.items)
		prefix := "msg_"
		if reasoning {
			prefix = "rs_"
		}
		p.keys[key] = i
		p.items = append(p.items, nil)
		p.states = append(p.states, &responsesProgressItem{id: newID(prefix), index: p.startIndex + i, reasoning: reasoning})
	}
	state := p.states[i]
	_, partIndex, partEvent, textEvent := state.fields()
	if !exists {
		if err := p.write(state.index, "response.output_item.added", map[string]any{"item": state.item(false)}); err != nil {
			return err
		}
		if err := p.write(state.index, partEvent+".added", map[string]any{"item_id": state.id, partIndex: 0, "part": state.part("")}); err != nil {
			return err
		}
	}
	state.text.WriteString(text)
	return p.write(state.index, textEvent+".delta", map[string]any{"item_id": state.id, partIndex: 0, "delta": text})
}

func (p *responsesProgress) finish() error {
	for i, state := range p.states {
		_, partIndex, partEvent, textEvent := state.fields()
		text := state.text.String()
		if err := p.write(state.index, textEvent+".done", map[string]any{"item_id": state.id, partIndex: 0, "text": text}); err != nil {
			return err
		}
		if err := p.write(state.index, partEvent+".done", map[string]any{"item_id": state.id, partIndex: 0, "part": state.part(text)}); err != nil {
			return err
		}
		item := state.item(true)
		if err := p.write(state.index, "response.output_item.done", map[string]any{"item": item}); err != nil {
			return err
		}
		b, err := json.Marshal(item)
		if err != nil {
			return err
		}
		p.items[i] = b
	}
	return nil
}

func (p *responsesProgress) write(index int, typ string, fields map[string]any) error {
	fields["type"] = typ
	fields["output_index"] = index
	b, err := json.Marshal(fields)
	if err != nil {
		return err
	}
	frame := append([]byte("event: "+typ+"\ndata: "), b...)
	return p.writer.WriteRaw(append(frame, '\n', '\n'))
}

func (s *responsesProgressItem) fields() (string, string, string, string) {
	if s.reasoning {
		return "summary", "summary_index", "response.reasoning_summary_part", "response.reasoning_summary_text"
	}
	return "content", "content_index", "response.content_part", "response.output_text"
}

func (s *responsesProgressItem) part(text string) map[string]any {
	if s.reasoning {
		return map[string]any{"type": "summary_text", "text": text}
	}
	return map[string]any{"type": "output_text", "text": text, "annotations": []any{}}
}

func (s *responsesProgressItem) item(done bool) map[string]any {
	item := map[string]any{"id": s.id, "type": "message", "role": "assistant", "phase": "commentary", "status": "in_progress"}
	if s.reasoning {
		item = map[string]any{"id": s.id, "type": "reasoning"}
	}
	field, _, _, _ := s.fields()
	item[field] = []any{}
	if done {
		item[field] = []any{s.part(s.text.String())}
		if !s.reasoning {
			item["status"] = "completed"
		}
	}
	return item
}

func (p *responsesProgress) output(final json.RawMessage) string {
	items := make([]json.RawMessage, 0, len(p.items)+1)
	if p.startIndex == 1 {
		items = append(items, final)
	}
	items = append(items, p.items...)
	if p.startIndex == 0 {
		items = append(items, final)
	}
	b, _ := json.Marshal(items)
	return string(b)
}

func responseTextItem(id, text string) json.RawMessage {
	b, _ := json.Marshal(map[string]any{
		"id": id, "type": "message", "role": "assistant", "status": "completed",
		"content": []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{}}},
	})
	return b
}
