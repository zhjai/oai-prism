package facade

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/oai-prism/oaiprism/internal/prism"
)

type clientResult struct {
	ID      string          `json:"call_id"`
	Name    string          `json:"name"`
	Kind    string          `json:"kind,omitempty"`
	Output  json.RawMessage `json:"output"`
	IsError bool            `json:"is_error,omitempty"`
}

// Adjacent user/result turns are grouped identically during replay and native
// fingerprinting. Calls and results remain JSON data with their original IDs.
type clientTurn struct {
	Role    string               `json:"role"`
	Content []prism.InputContent `json:"content,omitempty"`
	Calls   []clientCall         `json:"calls,omitempty"`
	Results []clientResult       `json:"results,omitempty"`
}

func toolText(turn clientTurn) string {
	var text strings.Builder
	for _, c := range turn.Content {
		if c.Type == "input_image" {
			text.WriteString(fmt.Sprintf("\n[image attachment %x]\n", sha256.Sum256([]byte(c.ImageURL))))
		} else {
			text.WriteString(c.Text)
		}
	}
	if len(turn.Calls) > 0 {
		b, _ := json.Marshal(turn.Calls)
		text.WriteString("\n[CLIENT CALLS] ")
		text.Write(b)
	}
	if len(turn.Results) > 0 {
		results := append([]clientResult(nil), turn.Results...)
		for i := range results {
			var blocks []map[string]any
			if json.Unmarshal(results[i].Output, &blocks) == nil {
				for j, b := range blocks {
					typ, _ := b["type"].(string)
					if typ == "image" || typ == "input_image" || typ == "image_url" {
						raw, _ := json.Marshal(b)
						blocks[j] = map[string]any{"type": "image", "attachment": fmt.Sprintf("%x", sha256.Sum256(raw))}
					}
				}
				results[i].Output, _ = json.Marshal(blocks)
			}
		}
		b, _ := json.Marshal(results)
		text.WriteString("\n[CLIENT RESULTS] ")
		text.Write(b)
	}
	return text.String()
}

func groupClientTurns(turns []clientTurn) []clientTurn {
	out := make([]clientTurn, 0, len(turns))
	for _, t := range turns {
		if t.Role == "user" && len(out) > 0 && out[len(out)-1].Role == "user" {
			last := &out[len(out)-1]
			if len(last.Content) > 0 && len(t.Content) > 0 {
				last.Content = append(last.Content, prism.InputContent{Type: prism.BlockInputText, Text: "\n"})
			}
			last.Content = append(last.Content, t.Content...)
			last.Results = append(last.Results, t.Results...)
		} else {
			out = append(out, t)
		}
	}
	return out
}

func chatClientTurns(raw json.RawMessage) ([]clientTurn, error) {
	var msgs []ChatMessage
	if json.Unmarshal(raw, &msgs) != nil {
		return nil, errors.New("invalid messages")
	}
	var turns []clientTurn
	for _, m := range msgs {
		role := strings.ToLower(strings.TrimSpace(m.Role))
		if role == "" {
			role = "user"
		}
		t := clientTurn{Role: role, Content: toInputContent(m.Content, role != "assistant")}
		switch role {
		case "system", "developer", "user":
		case "assistant":
			for _, c := range m.ToolCalls {
				if c.Type != "" && c.Type != "function" {
					return nil, errors.New("Chat supports function tool calls only")
				}
				args, err := uniqueJSON([]byte(c.Function.Arguments))
				if err != nil {
					return nil, errors.New("invalid replayed tool arguments")
				}
				if _, ok := args.(map[string]any); !ok {
					return nil, errors.New("replayed function arguments must be an object")
				}
				b, _ := json.Marshal(args)
				t.Calls = append(t.Calls, clientCall{ID: c.ID, Kind: "function", Name: c.Function.Name, Arguments: b})
			}
		case "tool":
			t.Role = "user"
			var err error
			t.Content, err = clientResultContent(m.Content.Raw())
			if err != nil {
				return nil, err
			}
			t.Results = []clientResult{{ID: m.ToolCallID, Name: m.Name, Kind: "function", Output: append(json.RawMessage(nil), m.Content.Raw()...)}}
		case "function":
			return nil, errors.New("legacy function messages are unsupported; use tool_call_id and role=tool")
		default:
			return nil, errors.New("unsupported message role")
		}
		turns = append(turns, t)
	}
	return groupClientTurns(turns), nil
}

func responsesClientTurns(raw json.RawMessage) ([]clientTurn, error) {
	trim := strings.TrimSpace(string(raw))
	if trim == "" || trim == "null" {
		return nil, nil
	}
	if strings.HasPrefix(trim, `"`) {
		var s string
		if json.Unmarshal(raw, &s) != nil {
			return nil, errors.New("invalid input")
		}
		return []clientTurn{{Role: "user", Content: []prism.InputContent{{Type: prism.BlockInputText, Text: s}}}}, nil
	}
	var items []json.RawMessage
	if strings.HasPrefix(trim, "{") {
		items = []json.RawMessage{raw}
	} else if json.Unmarshal(raw, &items) != nil {
		return nil, errors.New("invalid Responses input")
	}
	var turns []clientTurn
	for _, item := range items {
		var v struct {
			Type      string          `json:"type"`
			Role      string          `json:"role"`
			Phase     string          `json:"phase"`
			Content   StringOrArray   `json:"content"`
			ID        string          `json:"id"`
			CallID    string          `json:"call_id"`
			Name      string          `json:"name"`
			Arguments string          `json:"arguments"`
			Input     *string         `json:"input"`
			Output    json.RawMessage `json:"output"`
		}
		if json.Unmarshal(item, &v) != nil {
			return nil, errors.New("invalid Responses item")
		}
		if v.Phase == "commentary" || v.Type == "reasoning" {
			continue
		}
		switch v.Type {
		case "", "message":
			role := v.Role
			if role == "" {
				role = "user"
			}
			if role != "user" && role != "assistant" && !isSystemRole(role) {
				return nil, errors.New("invalid Responses message role")
			}
			content := toInputContent(v.Content, role != "assistant")
			// Responses output items may place assistant text before or after
			// calls. Replay a contiguous assistant output as one logical turn.
			if role == "assistant" && len(turns) > 0 && turns[len(turns)-1].Role == "assistant" {
				last := &turns[len(turns)-1]
				if len(last.Content) > 0 && len(content) > 0 {
					last.Content = append(last.Content, prism.InputContent{Type: prism.BlockInputText, Text: "\n"})
				}
				last.Content = append(last.Content, content...)
			} else {
				turns = append(turns, clientTurn{Role: role, Content: content})
			}
		case "function_call", "custom_tool_call":
			c := clientCall{ID: v.CallID, Name: v.Name, Kind: "function"}
			if v.Type == "custom_tool_call" {
				c.Kind = "custom"
				c.Input = v.Input
				if c.Input == nil {
					return nil, errors.New("missing custom input")
				}
			} else {
				args, err := uniqueJSON([]byte(v.Arguments))
				if err != nil {
					return nil, errors.New("invalid replayed arguments")
				}
				if _, ok := args.(map[string]any); !ok {
					return nil, errors.New("replayed arguments must be an object")
				}
				c.Arguments, _ = json.Marshal(args)
			}
			if len(turns) > 0 && turns[len(turns)-1].Role == "assistant" {
				turns[len(turns)-1].Calls = append(turns[len(turns)-1].Calls, c)
			} else {
				turns = append(turns, clientTurn{Role: "assistant", Calls: []clientCall{c}})
			}
		case "function_call_output", "custom_tool_call_output":
			content, err := clientResultContent(v.Output)
			if err != nil {
				return nil, err
			}
			kind := "function"
			if v.Type == "custom_tool_call_output" {
				kind = "custom"
			}
			turns = append(turns, clientTurn{Role: "user", Content: content, Results: []clientResult{{ID: v.CallID, Name: v.Name, Kind: kind, Output: v.Output}}})
		default:
			return nil, errors.New("unsupported Responses input item")
		}
	}
	return groupClientTurns(turns), nil
}

func clientResultContent(raw json.RawMessage) ([]prism.InputContent, error) {
	if len(raw) == 0 {
		return nil, errors.New("tool result output is required")
	}
	if strings.TrimSpace(string(raw)) == "null" {
		return nil, errors.New("tool output cannot be null")
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return nil, nil
	}
	var blocks []map[string]any
	if json.Unmarshal(raw, &blocks) != nil {
		return nil, errors.New("tool output must be a string or content block array")
	}
	var images []prism.InputContent
	for _, b := range blocks {
		typ, _ := b["type"].(string)
		switch typ {
		case "text", "input_text":
		case "input_image", "image_url", "image":
			url, detail := imageURLAndDetail(b)
			if typ == "image" {
				var err error
				url, err = anthropicToolImage(b)
				if err != nil {
					return nil, err
				}
				detail = "auto"
			}
			if url == "" {
				return nil, errors.New("tool image URL is required")
			}
			images = append(images, prism.InputContent{Type: "input_image", ImageURL: url, Detail: detail})
		default:
			return nil, errors.New("unsupported tool result content block")
		}
	}
	return images, nil
}

func anthropicToolImage(b map[string]any) (string, error) {
	s, _ := b["source"].(map[string]any)
	typ, _ := s["type"].(string)
	if typ == "url" {
		url, _ := s["url"].(string)
		return url, nil
	}
	if typ == "base64" {
		media, _ := s["media_type"].(string)
		data, _ := s["data"].(string)
		if !strings.HasPrefix(media, "image/") || data == "" {
			return "", errors.New("invalid image source")
		}
		return "data:" + media + ";base64," + data, nil
	}
	return "", errors.New("unsupported image source")
}

func anthropicClientTurns(raw json.RawMessage) ([]clientTurn, error) {
	var msgs []AnthropicMessage
	if json.Unmarshal(raw, &msgs) != nil {
		return nil, errors.New("invalid messages")
	}
	var turns []clientTurn
	for _, m := range msgs {
		if m.Role != "user" && m.Role != "assistant" {
			return nil, errors.New("invalid Anthropic role")
		}
		t := clientTurn{Role: m.Role}
		if strings.HasPrefix(strings.TrimSpace(string(m.Content.Raw())), `"`) {
			t.Content = toInputContent(m.Content, true)
			turns = append(turns, t)
			continue
		}
		var blocks []map[string]json.RawMessage
		if json.Unmarshal(m.Content.Raw(), &blocks) != nil {
			return nil, errors.New("invalid Anthropic content")
		}
		for _, b := range blocks {
			get := func(k string) string { var s string; _ = json.Unmarshal(b[k], &s); return s }
			switch get("type") {
			case "text":
				t.Content = append(t.Content, prism.InputContent{Type: prism.BlockInputText, Text: get("text")})
			case "image":
				var obj map[string]any
				data, _ := json.Marshal(b)
				_ = json.Unmarshal(data, &obj)
				url, err := anthropicToolImage(obj)
				if err != nil {
					return nil, err
				}
				t.Content = append(t.Content, prism.InputContent{Type: "input_image", ImageURL: url, Detail: "auto"})
			case "tool_use":
				if m.Role != "assistant" {
					return nil, errors.New("tool_use must belong to assistant")
				}
				args, err := uniqueJSON(b["input"])
				if err != nil {
					return nil, errors.New("invalid tool_use input")
				}
				if _, ok := args.(map[string]any); !ok {
					return nil, errors.New("tool_use input must be an object")
				}
				argJSON, _ := json.Marshal(args)
				t.Calls = append(t.Calls, clientCall{ID: get("id"), Name: get("name"), Kind: "function", Arguments: argJSON})
			case "tool_result":
				if m.Role != "user" {
					return nil, errors.New("tool_result must belong to user")
				}
				if len(b["content"]) == 0 {
					b["content"] = json.RawMessage(`""`)
				}
				content, err := clientResultContent(b["content"])
				if err != nil {
					return nil, err
				}
				var isError bool
				if len(b["is_error"]) > 0 && json.Unmarshal(b["is_error"], &isError) != nil {
					return nil, errors.New("is_error must be boolean")
				}
				t.Content = append(t.Content, content...)
				t.Results = append(t.Results, clientResult{ID: get("tool_use_id"), Kind: "function", Output: b["content"], IsError: isError})
			case "thinking", "redacted_thinking": // Not replayed as unsigned model text.
			default:
				return nil, errors.New("unsupported Anthropic content block")
			}
		}
		turns = append(turns, t)
	}
	return groupClientTurns(turns), nil
}

func validateClientTranscript(turns []clientTurn) error {
	seen := map[string]bool{}
	pending := map[string]clientCall{}
	for i := range turns {
		t := &turns[i]
		if len(pending) > 0 && (t.Role != "user" || len(t.Results) == 0) {
			return errors.New("all pending tool calls require results before continuing")
		}
		for j := range t.Results {
			r := &t.Results[j]
			call, ok := pending[r.ID]
			if r.ID == "" || !ok {
				return errors.New("unknown or duplicate tool result call_id")
			}
			if r.Name != "" && r.Name != call.Name {
				return errors.New("tool result name does not match its call")
			}
			if r.Kind != "" && r.Kind != call.Kind {
				return errors.New("tool result type does not match its call")
			}
			r.Name = call.Name
			delete(pending, r.ID)
		}
		if len(t.Results) > 0 && len(pending) > 0 {
			return errors.New("missing parallel tool results")
		}
		for _, c := range t.Calls {
			if c.ID == "" || len(c.ID) > 256 || seen[c.ID] || !clientToolName.MatchString(c.Name) {
				return errors.New("invalid or duplicate tool call ID/name")
			}
			seen[c.ID] = true
			pending[c.ID] = c
		}
	}
	if len(pending) > 0 {
		return errors.New("missing tool call results")
	}
	return nil
}

func (b *clientToolRequest) conversation(system string, limit int) ([]prism.InputItem, *nativeConversation, error) {
	var sys []string
	var turns []clientTurn
	for _, t := range b.turns {
		if isSystemRole(t.Role) {
			sys = append(sys, toolText(t))
		} else {
			turns = append(turns, t)
		}
	}
	if len(sys) == 0 && system != "" {
		sys = append(sys, system)
	}
	sys = append(sys, b.prompt())
	system = strings.Join(sys, "\n\n")
	if len(turns) == 0 || turns[len(turns)-1].Role != "user" {
		return nil, nil, errors.New("tool request must end with user input or tool results")
	}
	last := turns[len(turns)-1]
	currentText := toolText(last)
	current := prism.NewUserItem(currentText)
	for _, c := range last.Content {
		if c.Type == "input_image" {
			current.Content = append(current.Content, c)
		}
	}
	var history []historyEntry
	for _, t := range turns[:len(turns)-1] {
		history = append(history, historyEntry{speaker: speakerOf(t.Role), text: toolText(t)})
	}
	conv := &nativeConversation{system: system, history: history, current: current, currentText: currentText, extra: b.directive()}
	// History pruning removes whole exchanges. Never split a call/result pair or
	// truncate the current tool payload. Native seeding sees the complete entries.
	var exchanges []historyEntry
	var group strings.Builder
	for _, t := range turns[:len(turns)-1] {
		if t.Role == "user" && len(t.Results) == 0 && group.Len() > 0 {
			exchanges = append(exchanges, historyEntry{speaker: "User", text: group.String()})
			group.Reset()
		}
		group.WriteString(speakerOf(t.Role) + ": " + toolText(t) + "\n")
	}
	if group.Len() > 0 {
		exchanges = append(exchanges, historyEntry{speaker: "User", text: group.String()})
	}
	conv.seedGroups = append([]historyEntry{}, exchanges...)
	activeExchange := ""
	if len(last.Results) > 0 && len(exchanges) > 0 {
		activeExchange = "\n[Active tool exchange]\n" + exchanges[len(exchanges)-1].text
		exchanges = exchanges[:len(exchanges)-1]
	}
	fixed := len(system) + len(currentText) + len(b.directive()) + len(activeExchange)
	items := []prism.InputItem{prism.NewSystemItem(system + renderHistory(exchanges, historyBudget(limit, fixed)) + activeExchange + "\n" + b.directive()), current}
	return items, conv, nil
}

// Tool transcripts stay local to this Handler and tenant. Each response stores
// a complete immutable snapshot, so branching never reads the latest chain.
// No plaintext tool payloads are persisted to disk.
type clientToolSnapshot struct {
	Turns   []clientTurn
	Account string
	Model   string
	Created time.Time
	Size    int
}
type clientToolHistory struct {
	mu      sync.Mutex
	entries map[string]clientToolSnapshot
	bytes   int
}

const toolHistoryMaxBytes = 16 << 20
const toolHistoryMaxEntries = 256
const toolHistoryTTL = 30 * time.Minute
const toolHistoryTenantEntries = 64
const toolHistoryTenantBytes = 4 << 20

func cloneClientTurns(turns []clientTurn) []clientTurn {
	b, _ := json.Marshal(turns)
	var copy []clientTurn
	_ = json.Unmarshal(b, &copy)
	return copy
}
func toolSnapshotKey(tenant, id string) string { return tenant + "\x00" + id }
func (s *clientToolHistory) get(tenant, id string) (clientToolSnapshot, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.entries[toolSnapshotKey(tenant, id)]
	if !ok || time.Since(v.Created) > toolHistoryTTL {
		return clientToolSnapshot{}, false
	}
	v.Turns = cloneClientTurns(v.Turns)
	return v, true
}
func (s *clientToolHistory) put(tenant, id string, turns []clientTurn, account, model string) {
	data, _ := json.Marshal(turns)
	if len(data) > toolHistoryTenantBytes {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.entries == nil {
		s.entries = map[string]clientToolSnapshot{}
	}
	for key, v := range s.entries {
		if time.Since(v.Created) > toolHistoryTTL {
			delete(s.entries, key)
			s.bytes -= v.Size
		}
	}
	key := toolSnapshotKey(tenant, id)
	if old, ok := s.entries[key]; ok {
		s.bytes -= old.Size
		delete(s.entries, key)
	}
	tenantPrefix := tenant + "\x00"
	for {
		count, size := 0, 0
		oldest := ""
		var when time.Time
		for k, v := range s.entries {
			if strings.HasPrefix(k, tenantPrefix) {
				count++
				size += v.Size
				if oldest == "" || v.Created.Before(when) {
					oldest = k
					when = v.Created
				}
			}
		}
		if count < toolHistoryTenantEntries && size+len(data) <= toolHistoryTenantBytes {
			break
		}
		if oldest == "" {
			return
		}
		s.bytes -= s.entries[oldest].Size
		delete(s.entries, oldest)
	}
	for len(s.entries) >= toolHistoryMaxEntries || s.bytes+len(data) > toolHistoryMaxBytes {
		var oldest string
		var when time.Time
		for k, v := range s.entries {
			if oldest == "" || v.Created.Before(when) {
				oldest = k
				when = v.Created
			}
		}
		if oldest == "" {
			break
		}
		s.bytes -= s.entries[oldest].Size
		delete(s.entries, oldest)
	}
	var frozen []clientTurn
	_ = json.Unmarshal(data, &frozen)
	s.entries[key] = clientToolSnapshot{Turns: frozen, Account: account, Model: model, Created: time.Now(), Size: len(data)}
	s.bytes += len(data)
}

func clientAssistant(reply *clientToolReply) clientTurn {
	t := clientTurn{Role: "assistant", Calls: reply.Calls}
	if reply.Text != "" {
		t.Content = []prism.InputContent{{Type: prism.BlockInputText, Text: reply.Text}}
	}
	return t
}
