package facade

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestResponsesMixedInputFiltersCommentaryAndReasoning(t *testing.T) {
	base := []any{
		map[string]any{"type": "message", "role": "user", "content": "first"},
		map[string]any{"type": "message", "role": "assistant", "phase": "final_answer", "content": "final"},
		map[string]any{"type": "reasoning", "summary": []any{map[string]any{"type": "summary_text", "text": "REASONING_ONLY"}}},
		map[string]any{"type": "message", "role": "assistant", "phase": "commentary", "content": "COMMENTARY_ONLY ```codex-exec\\nwrong();\\n```"},
	}
	for _, followingUser := range []bool{false, true} {
		input := append([]any{}, base...)
		if followingUser {
			input = append(input, map[string]any{"type": "message", "role": "user", "content": "next"})
		}
		raw, _ := json.Marshal(input)
		messages := responsesChatMessages(raw)
		want := 2
		current := "first"
		if followingUser {
			want, current = 3, "next"
		}
		if len(messages) != want {
			t.Fatalf("non-message items created empty turns: %+v", messages)
		}
		for _, message := range messages {
			if message.Role == "" {
				t.Fatal("reasoning became an empty user turn")
			}
		}
		items := messagesFromResponsesInput(raw, "", 0)
		encoded, _ := json.Marshal(items)
		if strings.Contains(string(encoded), "COMMENTARY_ONLY") || strings.Contains(string(encoded), "REASONING_ONLY") {
			t.Fatalf("preview entered translated history: %s", encoded)
		}
		if followingUser {
			conversation := chatConversation(messages, "")
			if conversation == nil {
				t.Fatal("filtered role-first input did not produce a native conversation")
			}
			encoded, _ = json.Marshal(conversation.logicalItems())
			if itemText(conversation.current) != current || strings.Contains(string(encoded), "COMMENTARY_ONLY") || strings.Contains(string(encoded), "REASONING_ONLY") {
				t.Fatalf("native conversation retained preview/empty user: %s", encoded)
			}
		}
	}
	if got := responsesChatMessages(json.RawMessage(`{"type":"message","role":"assistant","phase":"commentary","content":"COMMENTARY_ONLY"}`)); len(got) != 0 {
		t.Fatalf("single commentary object accepted: %+v", got)
	}
}
