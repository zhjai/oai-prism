package facade

import (
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const testClientTools = `[{"type":"function","name":"lookup","parameters":{"type":"object","required":["city"],"properties":{"city":{"type":"string"}},"additionalProperties":false}}]`

func testClientBridge(t *testing.T, choice string, parallel bool) *clientToolRequest {
	t.Helper()
	tools, err := normalizeClientTools(json.RawMessage(testClientTools), "responses")
	if err != nil {
		t.Fatal(err)
	}
	b := &clientToolRequest{tools: tools, choice: choice, parallel: parallel}
	return b
}
func clientEnvelope(body string) string { return clientToolFence + "\n" + body + "\n```" }

func TestClientToolJSONDiagnosticsAndCRLF(t *testing.T) {
	b := testClientBridge(t, "required", false)
	body := `{"calls":[{"name":"lookup","arguments":{"city":"Paris\\\"\n中文"}}]}`
	reply, err := b.parseReply(clientToolFence + "\r\n" + body + "\r\n```")
	if err != nil || len(reply.Calls) != 1 {
		t.Fatalf("valid CRLF/escaped payload rejected: %v", err)
	}
	for _, tc := range []struct{ body, kind string }{
		{`{"secret":"PRIVATE_DATA","calls":[}`, "syntax"},
		{`{"secret":"PRIVATE_DATA","calls":[`, "unexpected_eof"},
		{`{"secret":"PRIVATE_DATA","secret":"PRIVATE_DATA"}`, "duplicate_key"},
		{`{"secret":"PRIVATE_DATA"} {}`, "trailing_data"},
	} {
		_, err := b.parseReply(clientEnvelope(tc.body))
		if !errors.Is(err, errClientToolOutput) || !strings.Contains(err.Error(), "json_kind="+tc.kind) || !strings.Contains(err.Error(), "sha256=") || strings.Contains(err.Error(), "PRIVATE_DATA") {
			t.Fatalf("unsafe or missing JSON metadata: %v", err)
		}
	}
}

func TestCastWorldEnvironmentActionNestedParameters(t *testing.T) {
	raw := json.RawMessage(`[{"type":"function","name":"environment_action","parameters":{"type":"object","properties":{"action_type":{"type":"string","enum":["REQUEST_STATE"]},"state_version":{"type":"integer"},"payload":{"type":"object","properties":{"section":{"type":"string"}},"required":["section"],"additionalProperties":false}},"required":["action_type","state_version","payload"],"additionalProperties":false}}]`)
	tools, err := normalizeClientTools(raw, "responses")
	if err != nil {
		t.Fatal(err)
	}
	b := &clientToolRequest{tools: tools, choice: "named", named: "environment_action"}
	body := `{"calls":[{"name":"environment_action","arguments":{"action_type":"REQUEST_STATE","state_version":0,"payload":{"section":"summary"}}}]}`
	reply, err := b.parseReply(clientEnvelope(body))
	if err != nil || len(reply.Calls) != 1 {
		t.Fatalf("nested action failed: %v", err)
	}
	var args struct {
		ActionType   string            `json:"action_type"`
		StateVersion int               `json:"state_version"`
		Payload      map[string]string `json:"payload"`
	}
	if err := json.Unmarshal(reply.Calls[0].Arguments, &args); err != nil || args.ActionType != "REQUEST_STATE" || args.StateVersion != 0 || args.Payload["section"] != "summary" {
		t.Fatal("action arguments changed")
	}
	_, err = b.parseReply(clientEnvelope(strings.Replace(body, `"section":"summary"`, `"section":"summary",`, 1)))
	if !errors.Is(err, errClientToolOutput) {
		t.Fatal("malformed action was repaired or accepted")
	}
}

func TestClientToolReplyContracts(t *testing.T) {
	good := `{"text":"checking","calls":[{"name":"lookup","arguments":{"city":"Paris"}}]}`
	for _, tc := range []struct {
		name, choice, body string
		parallel, valid    bool
	}{
		{"named", "named", good, true, true},
		{"required", "required", good, true, true},
		{"auto_text", "auto", "hello", true, true},
		{"none_text", "none", "hello", true, true},
		{"required_missing", "required", "hello", true, false},
		{"none_call", "none", good, true, false},
		{"unknown_name", "auto", `{"calls":[{"name":"other","arguments":{}}]}`, true, false},
		{"wrong_type", "auto", `{"calls":[{"name":"lookup","arguments":{"city":7}}]}`, true, false},
		{"missing_required", "auto", `{"calls":[{"name":"lookup","arguments":{}}]}`, true, false},
		{"extra_key", "auto", `{"calls":[{"name":"lookup","arguments":{"city":"P","extra":1}}]}`, true, false},
		{"duplicate_key", "auto", `{"calls":[{"name":"lookup","arguments":{"city":"P","city":"Q"}}]}`, true, false},
		{"parallel_off", "required", `{"calls":[{"name":"lookup","arguments":{"city":"P"}},{"name":"lookup","arguments":{"city":"Q"}}]}`, false, false},
		{"parallel_on", "required", `{"calls":[{"name":"lookup","arguments":{"city":"P"}},{"name":"lookup","arguments":{"city":"Q"}}]}`, true, true},
		{"named_one", "named", `{"calls":[{"name":"lookup","arguments":{"city":"P"}},{"name":"lookup","arguments":{"city":"Q"}}]}`, true, false},
		{"supplied_id", "auto", `{"calls":[{"id":"invented","name":"lookup","arguments":{"city":"P"}}]}`, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := testClientBridge(t, tc.choice, tc.parallel)
			b.named = "lookup"
			text := tc.body
			if strings.HasPrefix(text, "{") {
				text = clientEnvelope(text)
			}
			reply, err := b.parseReply(text)
			if tc.valid {
				if err != nil {
					t.Fatal(err)
				}
				if len(reply.Calls) > 0 && !strings.HasPrefix(reply.Calls[0].ID, "call_") {
					t.Fatal(reply)
				}
			} else if !errors.Is(err, errClientToolOutput) {
				t.Fatalf("expected explicit invalid output, got %#v %v", reply, err)
			}
		})
	}
	for _, text := range []string{clientToolFence + "\n{", clientEnvelope(good) + " leaked", clientEnvelope(good) + "\n" + clientEnvelope(good)} {
		if _, err := testClientBridge(t, "auto", true).parseReply(text); err == nil {
			t.Fatal("malformed envelope accepted")
		}
	}
}

func TestClientToolNormalizationAndChoice(t *testing.T) {
	for _, tc := range []struct {
		api, raw string
		valid    bool
	}{
		{"responses", testClientTools, true},
		{"responses", `[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}]`, true},
		{"chat", `[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}]`, true},
		{"messages", `[{"name":"lookup","input_schema":{"type":"object"}}]`, true},
		{"responses", `[{"type":"custom","name":"patch","format":{"type":"text"}}]`, true},
		{"responses", `[{"type":"custom","name":"patch","format":{"type":"grammar","definition":"x"}}]`, false},
		{"chat", `[{"type":"custom","name":"patch"}]`, false},
		{"responses", `[{"type":"web_search"}]`, false},
		{"messages", `[{"type":"bash_20250124","name":"bash"}]`, false},
		{"responses", `[{"type":"function","name":"lookup","parameters":{"$ref":"file:///etc/passwd"}}]`, false},
		{"responses", `[{"type":"function","name":"lookup","parameters":{"$ref":"http://127.0.0.1/private"}}]`, false},
		{"responses", `[{"type":"function","name":"lookup","parameters":{"type":17}}]`, false},
		{"responses", `[{"type":"function","name":"bad name"}]`, false},
		{"responses", `[{"type":"function","name":"same"},{"type":"function","name":"same"}]`, false},
	} {
		tools, err := normalizeClientTools(json.RawMessage(tc.raw), tc.api)
		if (err == nil) != tc.valid {
			t.Fatalf("%s %s: %v", tc.api, tc.raw, err)
		}
		if tc.valid && len(tools) != 1 {
			t.Fatal(tools)
		}
	}
	for _, raw := range []string{`"required"`, `"none"`, `"auto"`, `{"type":"function","name":"lookup"}`} {
		b := testClientBridge(t, "auto", true)
		if err := b.parseChoice(json.RawMessage(raw), json.RawMessage("false"), "responses"); err != nil || b.parallel {
			t.Fatalf("%s %v", raw, err)
		}
	}
	for _, raw := range []string{`"bad"`, `{"type":"function","name":"unknown"}`, `{"type":"custom","name":"lookup"}`} {
		if testClientBridge(t, "auto", true).parseChoice(json.RawMessage(raw), nil, "responses") == nil {
			t.Fatal("invalid choice accepted", raw)
		}
	}
	b := testClientBridge(t, "auto", true)
	if err := b.parseChoice(json.RawMessage(`{"type":"tool","name":"lookup","disable_parallel_tool_use":true}`), nil, "messages"); err != nil || b.parallel || b.named != "lookup" {
		t.Fatal(err, b)
	}
}

func TestClientToolReplayPreservesTaskAndAllResults(t *testing.T) {
	raw := json.RawMessage(`[{"role":"user","content":"ORIGINAL_TASK"},{"role":"assistant","tool_calls":[{"id":"a","type":"function","function":{"name":"lookup","arguments":"{\"city\":\"Paris\"}"}},{"id":"b","type":"function","function":{"name":"lookup","arguments":"{\"city\":\"Rome\"}"}}]},{"role":"tool","tool_call_id":"a","content":"RESULT_A"},{"role":"tool","tool_call_id":"b","content":"RESULT_B"}]`)
	turns, err := chatClientTurns(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err = validateClientTranscript(turns); err != nil {
		t.Fatal(err)
	}
	b := testClientBridge(t, "auto", true)
	b.turns = turns
	items, conv, err := b.conversation("SYSTEM", 96000)
	if err != nil {
		t.Fatal(err)
	}
	prompt := itemText(items[0]) + itemText(items[1])
	for _, s := range []string{"ORIGINAL_TASK", "RESULT_A", "RESULT_B", `"call_id":"a"`, `"call_id":"b"`} {
		if !strings.Contains(prompt, s) {
			t.Fatal("lost", s)
		}
	}
	if len(conv.history) != 2 || !strings.Contains(conv.currentText, "RESULT_A") || !strings.Contains(conv.currentText, "RESULT_B") {
		t.Fatal(conv)
	}
	// The assistant fingerprint used at commit is identical to client replay.
	prior := clientAssistant(&clientToolReply{Calls: turns[1].Calls})
	if toolText(prior) != toolText(turns[1]) {
		t.Fatal("assistant normalization is unstable")
	}
	for _, bad := range []string{strings.Replace(string(raw), `"tool_call_id":"b"`, `"tool_call_id":"a"`, 1), strings.Replace(string(raw), `"tool_call_id":"b"`, `"tool_call_id":"unknown"`, 1)} {
		ts, _ := chatClientTurns(json.RawMessage(bad))
		if validateClientTranscript(ts) == nil {
			t.Fatal("invalid result accepted")
		}
	}
	if validateClientTranscript(turns[:2]) == nil {
		t.Fatal("missing results accepted")
	}
}

func TestClientToolResponsesAndAnthropicReplay(t *testing.T) {
	responses := json.RawMessage(`[{"role":"user","content":"task"},{"type":"custom_tool_call","call_id":"c","name":"patch","input":"raw patch"},{"type":"custom_tool_call_output","call_id":"c","output":[{"type":"input_text","text":"RESULT"},{"type":"input_image","image_url":"https://example.test/image.png"}]}]`)
	turns, err := responsesClientTurns(responses)
	if err != nil {
		t.Fatal(err)
	}
	if err = validateClientTranscript(turns); err != nil {
		t.Fatal(err)
	}
	if turns[2].Content[0].Type != "input_image" || !strings.Contains(toolText(turns[2]), "RESULT") {
		t.Fatal(turns)
	}
	ant := json.RawMessage(`[{"role":"user","content":"task"},{"role":"assistant","content":[{"type":"tool_use","id":"t","name":"lookup","input":{"city":"Paris"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","is_error":true,"content":[{"type":"text","text":"ERR"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aGVsbG8="}}]}]}]`)
	turns, err = anthropicClientTurns(ant)
	if err != nil {
		t.Fatal(err)
	}
	if err = validateClientTranscript(turns); err != nil {
		t.Fatal(err)
	}
	if !turns[2].Results[0].IsError || !strings.Contains(turns[2].Content[0].ImageURL, "base64,") {
		t.Fatal(turns)
	}
	if strings.Contains(toolText(turns[2]), "aGVsbG8=") {
		t.Fatal("image data duplicated into prompt")
	}
}

func TestClientToolResponsesMixedAssistantItems(t *testing.T) {
	for _, items := range []string{
		`{"role":"assistant","content":"Checking"},{"type":"function_call","call_id":"a","name":"lookup","arguments":"{\"city\":\"Paris\"}"},{"type":"function_call","call_id":"b","name":"lookup","arguments":"{\"city\":\"Rome\"}"}`,
		`{"type":"function_call","call_id":"a","name":"lookup","arguments":"{\"city\":\"Paris\"}"},{"role":"assistant","content":"Checking"},{"type":"function_call","call_id":"b","name":"lookup","arguments":"{\"city\":\"Rome\"}"}`,
		`{"type":"function_call","call_id":"a","name":"lookup","arguments":"{\"city\":\"Paris\"}"},{"type":"function_call","call_id":"b","name":"lookup","arguments":"{\"city\":\"Rome\"}"},{"role":"assistant","content":"Checking"}`,
	} {
		raw := json.RawMessage(`[{"role":"user","content":"task"},` + items + `,{"type":"function_call_output","call_id":"a","output":"Paris result"},{"type":"function_call_output","call_id":"b","output":"Rome result"}]`)
		turns, err := responsesClientTurns(raw)
		if err != nil {
			t.Fatal(err)
		}
		if err = validateClientTranscript(turns); err != nil {
			t.Fatal(err)
		}
		if len(turns) != 3 || len(turns[1].Calls) != 2 || len(turns[2].Results) != 2 {
			t.Fatal("assistant output items were not grouped", turns)
		}
		canonical := clientAssistant(&clientToolReply{Text: "Checking", Calls: turns[1].Calls})
		if toolText(canonical) != toolText(turns[1]) {
			t.Fatal("item order changed native assistant fingerprint")
		}
	}
}

func TestClientToolSnapshotsIsolationBranchAndExpiry(t *testing.T) {
	var s clientToolHistory
	turns := []clientTurn{{Role: "assistant", Calls: []clientCall{{ID: "a", Kind: "function", Name: "lookup", Arguments: json.RawMessage(`{}`)}}}}
	s.put("tenant-a", "r1", turns, "account-a", "model-a")
	turns[0].Calls[0].Name = "mutated"
	got, ok := s.get("tenant-a", "r1")
	if !ok || got.Turns[0].Calls[0].Name != "lookup" {
		t.Fatal("mutable snapshot")
	}
	if _, ok = s.get("tenant-b", "r1"); ok {
		t.Fatal("cross-tenant lookup")
	}
	s.put("tenant-a", "r2", []clientTurn{{Role: "user"}}, "account-a", "model-a")
	got, _ = s.get("tenant-a", "r1")
	if got.Turns[0].Calls[0].ID != "a" {
		t.Fatal("branch overwritten")
	}
	s.mu.Lock()
	expired := s.entries[toolSnapshotKey("tenant-a", "r1")]
	expired.Created = time.Now().Add(-toolHistoryTTL - time.Second)
	s.entries[toolSnapshotKey("tenant-a", "r1")] = expired
	s.mu.Unlock()
	if _, ok = s.get("tenant-a", "r1"); ok {
		t.Fatal("expired snapshot accepted")
	}
}

func TestClientToolCodexRouting(t *testing.T) {
	r := httptest.NewRequest("POST", "/v1/responses", nil)
	for _, name := range []string{"execute_query", "exec_python", "shell_run", "apply_patch_v2", "exec_command"} {
		raw := map[string]json.RawMessage{"input": json.RawMessage(`"task"`), "tools": json.RawMessage(`[{"type":"function","name":"` + name + `","parameters":{"type":"object"}}]`)}
		if codexBridgeRequest(r, raw) {
			t.Fatal("generic tool captured", name)
		}
	}
	if codexBridgeRequest(r, map[string]json.RawMessage{"input": json.RawMessage(`[{"type":"custom_tool_call","name":"lookup","call_id":"c","input":"x"}]`)}) {
		t.Fatal("generic custom replay captured")
	}
}

func TestClientToolSeedNeverClipsPayload(t *testing.T) {
	payload := strings.Repeat("FULL_TOOL_OUTPUT", 1000)
	conv := &nativeConversation{history: []historyEntry{{speaker: "User", text: payload}}, seedGroups: []historyEntry{{speaker: "User", text: payload}, {speaker: "User", text: "small complete exchange"}}}
	nt := &nativeTurn{conv: conv}
	seeds := nt.seedTurns(8192)
	if len(seeds) != 0 {
		t.Fatal("generic context entered lossy legacy seed planner")
	}
	parts, err := splitPreservedTranscript(payload, 8192)
	if err != nil || strings.Join(parts, "") != payload {
		t.Fatal("tool result not fully preserved", err)
	}
}

func TestClientToolHistoryTenantQuota(t *testing.T) {
	var s clientToolHistory
	s.put("other", "keep", []clientTurn{{Role: "user"}}, "a", "m")
	for i := 0; i < toolHistoryTenantEntries+10; i++ {
		s.put("busy", strings.Repeat("x", i+1), []clientTurn{{Role: "user"}}, "a", "m")
	}
	if _, ok := s.get("other", "keep"); !ok {
		t.Fatal("busy tenant evicted other tenant")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for key := range s.entries {
		if strings.HasPrefix(key, "busy\x00") {
			n++
		}
	}
	if n > toolHistoryTenantEntries {
		t.Fatal("tenant count quota not enforced")
	}
}
