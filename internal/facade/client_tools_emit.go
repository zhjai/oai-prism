package facade

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/oai-prism/oaiprism/internal/middleware"
	"github.com/oai-prism/oaiprism/internal/prism"
	"github.com/oai-prism/oaiprism/internal/sse"
)

func clientChatCalls(calls []clientCall, stream bool) []ToolCall {
	out := make([]ToolCall, 0, len(calls))
	for i, c := range calls {
		tc := ToolCall{ID: c.ID, Type: "function", Function: ToolCallFunc{Name: c.Name, Arguments: string(c.Arguments)}}
		if stream {
			idx := i
			tc.Index = &idx
		}
		out = append(out, tc)
	}
	return out
}

func clientResponseItems(reply *clientToolReply) []map[string]any {
	out := make([]map[string]any, 0, len(reply.Calls)+1)
	if reply.Text != "" || len(reply.Calls) == 0 {
		var text map[string]any
		_ = json.Unmarshal(responseTextItem(newID("msg_"), reply.Text), &text)
		out = append(out, text)
	}
	for _, c := range reply.Calls {
		typ, prefix, field, value := "function_call", "fc_", "arguments", string(c.Arguments)
		if c.Kind == "custom" {
			typ, prefix, field, value = "custom_tool_call", "ctc_", "input", *c.Input
		}
		out = append(out, map[string]any{"type": typ, "id": newID(prefix), "call_id": c.ID, "name": c.Name, "status": "completed", field: value})
	}
	return out
}

func clientAnthropicBlocks(reply *clientToolReply) []map[string]any {
	out := make([]map[string]any, 0, len(reply.Calls)+1)
	if reply.Text != "" || len(reply.Calls) == 0 {
		out = append(out, map[string]any{"type": "text", "text": reply.Text})
	}
	for _, c := range reply.Calls {
		out = append(out, map[string]any{"type": "tool_use", "id": c.ID, "name": c.Name, "input": json.RawMessage(c.Arguments)})
	}
	return out
}

func (h *Handler) syncClientToolReply(w http.ResponseWriter, api, id, model string, created int64, reply *clientToolReply, res *RunResult) {
	usage := res.Usage
	if usage == nil {
		usage = outputOnlyUsage(res)
	}
	switch api {
	case "chat":
		msg := ChatMessage{Role: "assistant", ToolCalls: clientChatCalls(reply.Calls, false), ReasoningContent: res.Reasoning}
		if reply.Text != "" || len(reply.Calls) == 0 {
			msg.Content = stringContent(reply.Text)
		}
		writeJSON(w, http.StatusOK, ChatResponse{ID: id, Object: "chat.completion", Created: created, Model: model, Choices: []ChatChoice{{Index: 0, Message: msg, FinishReason: finishReason(msg.ToolCalls)}}, Usage: newChatUsage(usage), PrismConversationID: res.ConversationID})
	case "responses":
		writeJSON(w, http.StatusOK, map[string]any{"id": id, "object": "response", "created_at": created, "status": "completed", "model": model, "output": clientResponseItems(reply), "usage": newResponsesUsage(usage), "prism_conversation_id": res.ConversationID})
	case "messages":
		stop := "end_turn"
		if len(reply.Calls) > 0 {
			stop = "tool_use"
		}
		writeJSON(w, http.StatusOK, map[string]any{"id": id, "type": "message", "role": "assistant", "model": model, "content": clientAnthropicBlocks(reply), "stop_reason": stop, "stop_sequence": nil, "usage": AnthropicUsage{InputTokens: usage.InputTokens, OutputTokens: usage.OutputTokens}})
	}
}

type clientToolStream struct {
	writer   *sse.Writer
	mu       sync.Mutex
	sequence int
}

func (s *clientToolStream) event(typ string, fields map[string]any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	fields["type"] = typ
	if strings.HasPrefix(typ, "response.") {
		fields["sequence_number"] = s.sequence
		s.sequence++
	}
	b, err := json.Marshal(fields)
	if err != nil {
		return err
	}
	frame := append([]byte("event: "+typ+"\ndata: "), b...)
	return s.writer.WriteRaw(append(frame, '\n', '\n'))
}

func clientToolStreamCode(err error) string {
	if errors.Is(err, errContextRestore) {
		return "context_restore_failed"
	}
	if errors.Is(err, errClientToolOutput) {
		return "tool_bridge_invalid_output"
	}
	if errors.Is(err, ErrContextTooLarge) {
		return "context_length_exceeded"
	}
	return "upstream_error"
}

func (h *Handler) streamClientTools(w http.ResponseWriter, r *http.Request, runReq *RunRequest, b *clientToolRequest, api, id, model string, created int64, reply **clientToolReply, finish func(*RunResult), raw map[string]json.RawMessage) {
	setConversationHeader(w, streamConversationID(runReq))
	sw, err := sse.New(w)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "server_error", err.Error())
		return
	}
	defer sw.Close()
	s := &clientToolStream{writer: sw}
	response := func(status string) map[string]any {
		return map[string]any{"id": id, "object": "response", "created_at": created, "status": status, "model": model, "output": []any{}}
	}
	switch api {
	case "chat":
		if sw.WriteData(AppendChatChunk(nil, ChatChunkSpec{ID: id, Created: created, Model: model, Role: "assistant"})) != nil {
			return
		}
	case "responses":
		if s.event("response.created", map[string]any{"response": response("in_progress")}) != nil {
			return
		}
		if s.event("response.in_progress", map[string]any{"response": response("in_progress")}) != nil {
			return
		}
	case "messages":
		if sw.WriteRaw(AppendAnthropicEvent(nil, AnthropicEvent{Type: "message_start", MessageID: id, Model: model, Usage: &prism.Usage{InputTokens: countInputTokens(runReq.Input)}})) != nil {
			return
		}
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				var err error
				if api == "messages" {
					err = s.event("ping", map[string]any{})
				} else {
					err = sw.WriteRaw([]byte(": keepalive\n\n"))
				}
				if err != nil {
					cancel()
					return
				}
			}
		}
	}()
	var once sync.Once
	stopHeartbeat := func() { once.Do(func() { close(stop); wg.Wait() }) }
	defer stopHeartbeat()
	// Preview strings may contain tool JSON. Only signed/typed protocol output
	// is authoritative; tool mode uses heartbeats until parsing final res.Text.
	// Keeping previews out also prevents partial fence tokens from leaking.
	if runReq.Deadline == 0 {
		runReq.Deadline = h.cfg.Facade.MaxPollTimeout
	}
	res, runErr := h.runner.Run(ctx, runReq, nil)
	bindLogResult(r, res)
	stopHeartbeat()
	if runErr != nil {
		if errors.Is(runErr, context.Canceled) {
			return
		}
		code := clientToolStreamCode(runErr)
		message := runErr.Error()
		if errors.Is(runErr, errContextRestore) {
			message = contextRestoreMessage
		}
		middleware.RecordLogError(r, "client tools stream failed: %s", code)
		switch api {
		case "chat":
			data, _ := json.Marshal(map[string]any{"error": map[string]any{"type": "upstream_error", "code": code, "message": message}})
			_ = sw.WriteData(data)
			_ = sw.Done()
		case "responses":
			resp := response("failed")
			resp["error"] = map[string]any{"code": code, "message": message}
			_ = s.event("response.failed", map[string]any{"response": resp})
		case "messages":
			typ := "api_error"
			if !errors.Is(runErr, errContextRestore) && errors.Is(runErr, ErrContextTooLarge) {
				typ, message = "invalid_request_error", anthropicTooLongMessage(runErr)
			}
			_ = s.event("error", map[string]any{"error": map[string]any{"type": typ, "code": code, "message": message}})
		}
		return
	}
	finish(res)
	out := *reply
	usage := res.Usage
	if usage == nil {
		usage = outputOnlyUsage(res)
	}
	switch api {
	case "chat":
		if out.Text != "" {
			if sw.WriteData(AppendChatChunk(nil, ChatChunkSpec{ID: id, Created: created, Model: model, Content: out.Text})) != nil {
				return
			}
		}
		calls := clientChatCalls(out.Calls, true)
		if len(calls) > 0 {
			if sw.WriteData(AppendChatChunk(nil, ChatChunkSpec{ID: id, Created: created, Model: model, ToolCalls: calls})) != nil {
				return
			}
		}
		if sw.WriteData(AppendChatChunk(nil, ChatChunkSpec{ID: id, Created: created, Model: model, HasFinish: true, Finish: finishReason(calls), ConversationID: res.ConversationID})) != nil {
			return
		}
		var so StreamOptions
		_ = json.Unmarshal(raw["stream_options"], &so)
		if so.IncludeUsage {
			_ = sw.WriteData(AppendChatChunk(nil, ChatChunkSpec{ID: id, Created: created, Model: model, EmptyChoices: true, Usage: usage}))
		}
		_ = sw.Done()
	case "responses":
		items := clientResponseItems(out)
		for i, item := range items {
			itemID, _ := item["id"].(string)
			typ, _ := item["type"].(string)
			added := map[string]any{}
			for k, v := range item {
				added[k] = v
			}
			added["status"] = "in_progress"
			if typ == "message" {
				added["content"] = []any{}
			} else if typ == "function_call" {
				added["arguments"] = ""
			} else {
				added["input"] = ""
			}
			if s.event("response.output_item.added", map[string]any{"output_index": i, "item": added}) != nil {
				return
			}
			if typ == "message" {
				part := map[string]any{"type": "output_text", "text": "", "annotations": []any{}}
				if s.event("response.content_part.added", map[string]any{"output_index": i, "item_id": itemID, "content_index": 0, "part": part}) != nil {
					return
				}
				if s.event("response.output_text.delta", map[string]any{"output_index": i, "item_id": itemID, "content_index": 0, "delta": out.Text}) != nil {
					return
				}
				if s.event("response.output_text.done", map[string]any{"output_index": i, "item_id": itemID, "content_index": 0, "text": out.Text}) != nil {
					return
				}
				part["text"] = out.Text
				if s.event("response.content_part.done", map[string]any{"output_index": i, "item_id": itemID, "content_index": 0, "part": part}) != nil {
					return
				}
			} else {
				field, event := "arguments", "response.function_call_arguments"
				if typ == "custom_tool_call" {
					field, event = "input", "response.custom_tool_call_input"
				}
				if s.event(event+".delta", map[string]any{"output_index": i, "item_id": itemID, "delta": item[field]}) != nil {
					return
				}
				if s.event(event+".done", map[string]any{"output_index": i, "item_id": itemID, field: item[field]}) != nil {
					return
				}
			}
			if s.event("response.output_item.done", map[string]any{"output_index": i, "item": item}) != nil {
				return
			}
		}
		resp := response("completed")
		resp["output"] = items
		resp["usage"] = newResponsesUsage(usage)
		resp["prism_conversation_id"] = res.ConversationID
		_ = s.event("response.completed", map[string]any{"response": resp})
	case "messages":
		for i, block := range clientAnthropicBlocks(out) {
			start := map[string]any{}
			for k, v := range block {
				start[k] = v
			}
			delta := map[string]any{}
			if block["type"] == "tool_use" {
				start["input"] = map[string]any{}
				delta = map[string]any{"type": "input_json_delta", "partial_json": string(block["input"].(json.RawMessage))}
			} else {
				start["text"] = ""
				delta = map[string]any{"type": "text_delta", "text": block["text"]}
			}
			if s.event("content_block_start", map[string]any{"index": i, "content_block": start}) != nil {
				return
			}
			if s.event("content_block_delta", map[string]any{"index": i, "delta": delta}) != nil {
				return
			}
			if s.event("content_block_stop", map[string]any{"index": i}) != nil {
				return
			}
		}
		stop := "end_turn"
		if len(out.Calls) > 0 {
			stop = "tool_use"
		}
		if s.event("message_delta", map[string]any{"delta": map[string]any{"stop_reason": stop, "stop_sequence": nil}, "usage": AnthropicUsage{InputTokens: usage.InputTokens, OutputTokens: usage.OutputTokens}}) != nil {
			return
		}
		_ = s.event("message_stop", map[string]any{})
	}
}
