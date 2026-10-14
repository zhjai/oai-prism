package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/oai-prism/oaiprism/internal/config"
)

const toolBridgeReply = "```prism-client-tools\n" + `{"text":"Checking","calls":[{"name":"lookup","arguments":{"city":"Paris"}}]}` + "\n```"

func clientToolBody(api string, stream bool) map[string]any {
	fn := map[string]any{"name": "lookup", "description": "Find a city", "parameters": map[string]any{"type": "object", "properties": map[string]any{"city": map[string]any{"type": "string"}}, "required": []string{"city"}, "additionalProperties": false}}
	body := map[string]any{"model": "gpt-5", "stream": stream}
	switch api {
	case "chat/completions":
		body["messages"] = []any{map[string]any{"role": "user", "content": "ORIGINAL_TASK Paris"}}
		body["tools"] = []any{map[string]any{"type": "function", "function": fn}}
		body["tool_choice"] = map[string]any{"type": "function", "function": map[string]any{"name": "lookup"}}
		body["parallel_tool_calls"] = false
	case "responses":
		fn["type"] = "function"
		body["input"] = "ORIGINAL_TASK Paris"
		body["tools"] = []any{fn}
		body["tool_choice"] = map[string]any{"type": "function", "name": "lookup"}
		body["parallel_tool_calls"] = false
	case "messages":
		body["messages"] = []any{map[string]any{"role": "user", "content": "ORIGINAL_TASK Paris"}}
		body["max_tokens"] = 512
		body["tools"] = []any{map[string]any{"name": "lookup", "input_schema": fn["parameters"]}}
		body["tool_choice"] = map[string]any{"type": "tool", "name": "lookup", "disable_parallel_tool_use": true}
	}
	return body
}

func postClientTool(t *testing.T, url, api string, body map[string]any) (int, string) {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return doLocal(t, http.MethodPost, url+"/v1/"+api, string(data), map[string]string{"Content-Type": "application/json"})
}

func toolSSE(t *testing.T, out string) []map[string]any {
	t.Helper()
	var events []map[string]any
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			continue
		}
		var ev map[string]any
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			t.Fatal(err)
		}
		events = append(events, ev)
	}
	return events
}

func TestClientToolsE2E_AllProtocolsRoundTrip(t *testing.T) {
	for _, api := range []string{"chat/completions", "responses", "messages"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", api, stream), func(t *testing.T) {
				up := &fakeUpstream{t: t, replyParts: []string{toolBridgeReply}, finalOnlyPayload: true}
				ts, _ := newTestServer(t, up, goodAccount(), nil)
				body := clientToolBody(api, stream)
				code, out := postClientTool(t, ts.URL, api, body)
				if code != 200 {
					t.Fatalf("%d %s", code, out)
				}
				if strings.Contains(out, "prism-client-tools") {
					t.Fatal("bridge fence leaked")
				}
				var result map[string]any
				callID, responseID := "", ""
				if !stream {
					if json.Unmarshal([]byte(out), &result) != nil {
						t.Fatal(out)
					}
				} else {
					events := toolSSE(t, out)
					switch api {
					case "chat/completions":
						for _, e := range events {
							choices, _ := e["choices"].([]any)
							if len(choices) == 0 {
								continue
							}
							c := choices[0].(map[string]any)
							d, _ := c["delta"].(map[string]any)
							if calls, ok := d["tool_calls"].([]any); ok {
								call := calls[0].(map[string]any)
								callID = call["id"].(string)
								if call["index"] != float64(0) || call["function"].(map[string]any)["arguments"] != `{"city":"Paris"}` {
									t.Fatal(call)
								}
							}
						}
						if !strings.Contains(out, `"finish_reason":"tool_calls"`) || !strings.Contains(out, "[DONE]") {
							t.Fatal(out)
						}
					case "responses":
						stage := 0
						added := map[string]float64{}
						for n, e := range events {
							if e["sequence_number"] != float64(n) {
								t.Fatalf("nonmonotonic sequence %#v", e)
							}
							typ, _ := e["type"].(string)
							if typ == "response.output_item.added" {
								item := e["item"].(map[string]any)
								added[item["id"].(string)] = e["output_index"].(float64)
								if item["type"] == "function_call" {
									if item["arguments"] != "" || item["status"] != "in_progress" {
										t.Fatal(item)
									}
									stage = 1
								}
							}
							if typ == "response.function_call_arguments.delta" {
								if stage != 1 || e["delta"] != `{"city":"Paris"}` {
									t.Fatal(e)
								}
								stage = 2
							}
							if typ == "response.function_call_arguments.done" {
								if stage != 2 {
									t.Fatal(e)
								}
								stage = 3
							}
							if typ == "response.output_item.done" {
								item := e["item"].(map[string]any)
								if added[item["id"].(string)] != e["output_index"] {
									t.Fatal(e)
								}
								if item["type"] == "function_call" {
									if stage != 3 {
										t.Fatal(e)
									}
									stage = 4
								}
							}
							if typ == "response.completed" {
								result = e["response"].(map[string]any)
							}
						}
						if stage != 4 || result == nil {
							t.Fatal(out)
						}
					case "messages":
						for _, e := range events {
							if e["type"] == "content_block_start" {
								block := e["content_block"].(map[string]any)
								if block["type"] == "tool_use" {
									callID = block["id"].(string)
									if e["index"] != float64(1) {
										t.Fatal(e)
									}
								}
							}
							if e["type"] == "content_block_delta" {
								d := e["delta"].(map[string]any)
								if d["type"] == "input_json_delta" && d["partial_json"] != `{"city":"Paris"}` {
									t.Fatal(d)
								}
							}
						}
						if !strings.Contains(out, `"stop_reason":"tool_use"`) || !strings.Contains(out, "message_stop") {
							t.Fatal(out)
						}
					}
				}
				switch api {
				case "chat/completions":
					if !stream {
						c := result["choices"].([]any)[0].(map[string]any)
						if c["finish_reason"] != "tool_calls" {
							t.Fatal(c)
						}
						call := c["message"].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)
						callID = call["id"].(string)
					}
				case "responses":
					responseID = result["id"].(string)
					items := result["output"].([]any)
					call := items[len(items)-1].(map[string]any)
					if call["type"] != "function_call" || call["name"] != "lookup" || call["arguments"] != `{"city":"Paris"}` {
						t.Fatal(call)
					}
					callID = call["call_id"].(string)
					if callID == call["id"] {
						t.Fatal("item ID must differ from call ID")
					}
				case "messages":
					if !stream {
						if result["stop_reason"] != "tool_use" {
							t.Fatal(result)
						}
						call := result["content"].([]any)[1].(map[string]any)
						callID = call["id"].(string)
					}
				}
				if callID == "" {
					t.Fatal("missing real call ID")
				}
				sys, _ := requireSystemUser(t, upstreamInput(t, up, 0))
				if !strings.Contains(sys, "lookup") || !strings.Contains(sys, "city") || !strings.Contains(sys, "MUST request exactly one") {
					t.Fatal("tool declarations/choice absent", sys)
				}
				up.mu.Lock()
				meta := up.startBodies[0]["metadata"].(map[string]any)
				_, leaked := meta["tools"]
				_, parallelLeak := up.startBodies[0]["parallel_tool_calls"]
				up.mu.Unlock()
				if leaked || parallelLeak {
					t.Fatal("client-only fields leaked upstream")
				}
				cid := startConv(t, up, 0)
				up.replyParts = []string{"RESULT_ACCEPTED"}
				body = clientToolBody(api, false)
				body["tool_choice"] = "auto"
				switch api {
				case "chat/completions":
					body["messages"] = []any{map[string]any{"role": "user", "content": "ORIGINAL_TASK Paris"}, map[string]any{"role": "assistant", "content": "Checking", "tool_calls": []any{map[string]any{"id": callID, "type": "function", "function": map[string]any{"name": "lookup", "arguments": `{"city":"Paris"}`}}}}, map[string]any{"role": "tool", "tool_call_id": callID, "content": "SENTINEL_RESULT"}}
				case "responses":
					body["previous_response_id"] = responseID
					body["input"] = []any{map[string]any{"type": "function_call_output", "call_id": callID, "output": "SENTINEL_RESULT"}}
				case "messages":
					body["tool_choice"] = map[string]any{"type": "auto"}
					body["messages"] = []any{map[string]any{"role": "user", "content": "ORIGINAL_TASK Paris"}, map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "text", "text": "Checking"}, map[string]any{"type": "tool_use", "id": callID, "name": "lookup", "input": map[string]any{"city": "Paris"}}}}, map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": callID, "content": "SENTINEL_RESULT"}}}}
				}
				code, out = postClientTool(t, ts.URL, api, body)
				if code != 200 || !strings.Contains(out, "RESULT_ACCEPTED") {
					t.Fatalf("followup %d: %s", code, out)
				}
				if cid == "" || startConv(t, up, 1) != cid {
					t.Fatal("tool round did not continue original conversation", cid, startConv(t, up, 1))
				}
				_, current := requireSystemUser(t, upstreamInput(t, up, 1))
				if !strings.Contains(current, "SENTINEL_RESULT") || !strings.Contains(current, callID) {
					t.Fatal("result/ID lost", current)
				}
			})
		}
	}
}

func TestClientToolsE2E_FailExplicitlyWithoutRetry(t *testing.T) {
	for _, api := range []string{"chat/completions", "responses", "messages"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/%t", api, stream), func(t *testing.T) {
				up := &fakeUpstream{t: t, replyParts: []string{"Tools unavailable."}}
				ts, _ := newTestServer(t, up, goodAccount(), nil)
				code, out := postClientTool(t, ts.URL, api, clientToolBody(api, stream))
				want := 502
				if stream {
					want = 200
				}
				if code != want || !strings.Contains(out, "tool_bridge_invalid_output") {
					t.Fatalf("%d: %s", code, out)
				}
				if strings.Contains(out, "response.completed") || strings.Contains(out, `"finish_reason":"stop"`) || strings.Contains(out, "message_stop") {
					t.Fatal("failure presented as success")
				}
				up.mu.Lock()
				n := len(up.startBodies)
				up.mu.Unlock()
				if n != 1 {
					t.Fatal("invalid output was retried", n)
				}
			})
		}
	}
}

func TestClientToolsE2E_InvalidInputsNoUpstream(t *testing.T) {
	up := &fakeUpstream{t: t}
	ts, _ := newTestServer(t, up, goodAccount(), nil)
	for _, body := range []map[string]any{
		{"model": "gpt-5", "input": "task", "tools": []any{map[string]any{"type": "web_search"}}},
		{"model": "gpt-5", "input": "task", "tools": []any{}, "tool_choice": "required"},
		{"model": "gpt-5", "input": []any{map[string]any{"type": "function_call_output", "call_id": "bad", "output": "x"}}, "previous_response_id": "missing"},
	} {
		code, out := postClientTool(t, ts.URL, "responses", body)
		if code != 400 {
			t.Fatalf("%d %s", code, out)
		}
	}
	up.mu.Lock()
	defer up.mu.Unlock()
	if len(up.startBodies) != 0 || up.projectCount != 0 {
		t.Fatal("invalid tool input caused side effects")
	}
}

func TestClientToolsE2E_ExplicitAccountAndPromptLimit(t *testing.T) {
	up := &fakeUpstream{t: t, replyParts: []string{toolBridgeReply}}
	accounts := []config.AccountConfig{{ID: "a", AccessToken: "a-token", MaxConcurrency: 2}, {ID: "b", AccessToken: "b-token", MaxConcurrency: 2}}
	ts, _ := newTestServer(t, up, accounts, nil)
	body := clientToolBody("responses", false)
	data, _ := json.Marshal(body)
	code, out := doLocal(t, http.MethodPost, ts.URL+"/v1/responses", string(data), map[string]string{"Content-Type": "application/json", "X-Oaiprism-Account": "b"})
	if code != 200 {
		t.Fatalf("%d %s", code, out)
	}
	up.mu.Lock()
	auth := up.startAuths[0]
	up.mu.Unlock()
	if auth != "Bearer b-token" {
		t.Fatal("explicit account ignored", auth)
	}
	body["input"] = strings.Repeat("CURRENT_PAYLOAD ", 70000)
	code, out = postClientTool(t, ts.URL, "responses", body)
	if code != 400 || !strings.Contains(out, "context_length_exceeded") {
		t.Fatalf("oversize %d %s", code, out)
	}
	up.mu.Lock()
	defer up.mu.Unlock()
	if len(up.startBodies) != 1 {
		t.Fatal("oversize request reached upstream")
	}
}

func TestClientToolsE2E_ResponsesCustomText(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			up := &fakeUpstream{t: t, replyParts: []string{"```prism-client-tools\n" + `{"calls":[{"name":"submit_patch","input":"line1\nline2"}]}` + "\n```"}}
			ts, _ := newTestServer(t, up, goodAccount(), nil)
			body := map[string]any{"model": "gpt-5", "input": "task", "stream": stream, "tools": []any{map[string]any{"type": "custom", "name": "submit_patch", "format": map[string]any{"type": "text"}}}, "tool_choice": map[string]any{"type": "custom", "name": "submit_patch"}}
			code, out := postClientTool(t, ts.URL, "responses", body)
			if code != 200 {
				t.Fatalf("%d %s", code, out)
			}
			var resp map[string]any
			if stream {
				stage := 0
				for _, ev := range toolSSE(t, out) {
					switch ev["type"] {
					case "response.custom_tool_call_input.delta":
						if ev["delta"] != "line1\nline2" || ev["output_index"] != float64(0) {
							t.Fatal(ev)
						}
						stage = 1
					case "response.custom_tool_call_input.done":
						if stage != 1 || ev["input"] != "line1\nline2" {
							t.Fatal(ev)
						}
						stage = 2
					case "response.completed":
						resp = ev["response"].(map[string]any)
					}
				}
				if stage != 2 {
					t.Fatal(out)
				}
			} else {
				_ = json.Unmarshal([]byte(out), &resp)
			}
			call := resp["output"].([]any)[0].(map[string]any)
			if call["type"] != "custom_tool_call" || call["input"] != "line1\nline2" {
				t.Fatal(call)
			}
			up.replyParts = []string{"PATCH_ACCEPTED"}
			body["stream"] = false
			body["tool_choice"] = "none"
			body["previous_response_id"] = resp["id"]
			body["input"] = []any{map[string]any{"type": "custom_tool_call_output", "call_id": call["call_id"], "output": "done"}}
			code, out = postClientTool(t, ts.URL, "responses", body)
			if code != 200 || !strings.Contains(out, "PATCH_ACCEPTED") {
				t.Fatalf("%d %s", code, out)
			}
		})
	}
}

func TestClientToolsE2E_ResponseSnapshotsBranchAndTenant(t *testing.T) {
	up := &fakeUpstream{t: t, replyParts: []string{toolBridgeReply}}
	ts, _ := newTestServer(t, up, goodAccount(), func(c *config.Config) { c.Facade.APIKeys = []string{"synthetic-key-a", "synthetic-key-b"} })
	post := func(body map[string]any, key string) (int, string) {
		t.Helper()
		data, _ := json.Marshal(body)
		return doLocal(t, http.MethodPost, ts.URL+"/v1/responses", string(data), map[string]string{"Content-Type": "application/json", "Authorization": "Bearer " + key})
	}
	first := clientToolBody("responses", false)
	first["instructions"] = "FIRST_TURN_ONLY"
	code, out := post(first, "synthetic-key-a")
	if code != 200 {
		t.Fatalf("%d %s", code, out)
	}
	var reply map[string]any
	_ = json.Unmarshal([]byte(out), &reply)
	call := reply["output"].([]any)[1].(map[string]any)
	up.replyParts = []string{"CONTINUED"}
	follow := clientToolBody("responses", false)
	follow["tool_choice"] = "auto"
	follow["previous_response_id"] = reply["id"]
	for n, result := range []string{"BRANCH_A", "BRANCH_B"} {
		follow["input"] = []any{map[string]any{"type": "function_call_output", "call_id": call["call_id"], "output": result}}
		code, out = post(follow, "synthetic-key-a")
		if code != 200 {
			t.Fatalf("branch%d %d %s", n, code, out)
		}
		sys, current := requireSystemUser(t, upstreamInput(t, up, n+1))
		if !strings.Contains(current, result) || strings.Contains(sys, "FIRST_TURN_ONLY") {
			t.Fatalf("branch/instruction leak: %s %s", sys, current)
		}
		if n == 1 && (strings.Contains(sys, "BRANCH_A") || strings.Contains(current, "BRANCH_A")) {
			t.Fatal("branch history overwritten")
		}
	}
	code, out = post(follow, "synthetic-key-b")
	if code != 400 || !strings.Contains(out, "previous_response_not_found") {
		t.Fatalf("cross-tenant %d %s", code, out)
	}
	up.mu.Lock()
	defer up.mu.Unlock()
	if len(up.startBodies) != 3 {
		t.Fatal("cross-tenant request reached upstream")
	}
}

func TestClientToolsE2E_PlainResponseThenTools(t *testing.T) {
	up := &fakeUpstream{t: t, replyParts: []string{"INITIAL_REPLY"}}
	ts, _ := newTestServer(t, up, goodAccount(), nil)
	code, out := postClientTool(t, ts.URL, "responses", map[string]any{"model": "gpt-5", "input": "ORIGINAL_PLAIN_TASK"})
	if code != 200 {
		t.Fatal(out)
	}
	var first map[string]any
	_ = json.Unmarshal([]byte(out), &first)
	up.replyParts = []string{toolBridgeReply}
	body := clientToolBody("responses", false)
	body["input"] = "Now call lookup"
	body["previous_response_id"] = first["id"]
	code, out = postClientTool(t, ts.URL, "responses", body)
	if code != 200 || !strings.Contains(out, `"type":"function_call"`) {
		t.Fatalf("%d %s", code, out)
	}
	if startConv(t, up, 0) != startConv(t, up, 1) {
		t.Fatal("adding tools lost native conversation")
	}
}

func TestClientToolsE2E_NoToolsKeepPlainStreaming(t *testing.T) {
	for _, api := range []string{"chat/completions", "responses", "messages"} {
		for _, choice := range []any{nil, "auto", "none"} {
			t.Run(fmt.Sprintf("%s/%v", api, choice), func(t *testing.T) {
				up := &fakeUpstream{t: t, replyParts: []string{"first", " then second"}}
				ts, _ := newTestServer(t, up, goodAccount(), func(c *config.Config) { c.Facade.PlatformNotice = false })
				body := map[string]any{"model": "gpt-5", "stream": true, "tool_choice": choice}
				if api == "responses" {
					body["input"] = "plain task"
				} else {
					body["messages"] = []any{map[string]any{"role": "user", "content": "plain task"}}
					if api == "messages" {
						body["max_tokens"] = 64
						if choice != nil {
							body["tool_choice"] = map[string]any{"type": choice}
						}
					}
				}
				code, out := postClientTool(t, ts.URL, api, body)
				if code != 200 {
					t.Fatalf("%d %s", code, out)
				}
				dataEvents := 0
				for _, event := range toolSSE(t, out) {
					switch api {
					case "responses":
						if event["type"] == "response.output_text.delta" {
							dataEvents++
						}
					case "messages":
						if event["type"] == "content_block_delta" {
							dataEvents++
						}
					case "chat/completions":
						choices, _ := event["choices"].([]any)
						if len(choices) > 0 {
							delta, _ := choices[0].(map[string]any)["delta"].(map[string]any)
							if delta["content"] != nil {
								dataEvents++
							}
						}
					}
				}
				if dataEvents < 2 {
					t.Fatalf("plain output was buffered: %s", out)
				}
				sys := ""
				for _, item := range upstreamInput(t, up, 0) {
					if item.role == "system" {
						sys += item.text
					}
				}
				if strings.Contains(sys, "client_tools") || strings.Contains(sys, "gateway_notice") {
					t.Fatal("plain request acquired tool/platform prompt")
				}
				if api == "responses" {
					var id any
					for _, e := range toolSSE(t, out) {
						if e["type"] == "response.completed" {
							id = e["response"].(map[string]any)["id"]
						}
					}
					body["previous_response_id"] = id
					body["input"] = "plain followup"
					code, out = postClientTool(t, ts.URL, api, body)
					if code != 200 {
						t.Fatal(out)
					}
					count := 0
					for _, e := range toolSSE(t, out) {
						if e["type"] == "response.output_text.delta" {
							count++
						}
					}
					if count < 2 {
						t.Fatal("previous_response_id changed plain streaming")
					}
					sys = ""
					for _, item := range upstreamInput(t, up, 1) {
						if item.role == "system" {
							sys += item.text
						}
					}
					if strings.Contains(sys, "client_tools") || strings.Contains(sys, "gateway_notice") {
						t.Fatal("plain followup acquired tool prompt")
					}
				}
			})
		}
	}
}

func TestClientToolsE2E_PartialSnapshotsAndStoreFalse(t *testing.T) {
	up := &fakeUpstream{t: t, replyParts: []string{"plain"}}
	ts, _ := newTestServer(t, up, goodAccount(), nil)
	for _, initial := range []map[string]any{{"model": "gpt-5", "input": "task", "previous_response_id": "unknown-history"}, {"model": "gpt-5", "input": "task", "store": false}} {
		code, out := postClientTool(t, ts.URL, "responses", initial)
		if code != 200 {
			t.Fatal(out)
		}
		var first map[string]any
		_ = json.Unmarshal([]byte(out), &first)
		body := clientToolBody("responses", false)
		body["previous_response_id"] = first["id"]
		code, out = postClientTool(t, ts.URL, "responses", body)
		if code != 400 || !strings.Contains(out, "previous_response_not_found") {
			t.Fatalf("partial/nonstored history falsely recovered %d %s", code, out)
		}
	}
}

func TestClientToolsE2E_RespectsPlatformNoticeSetting(t *testing.T) {
	up := &fakeUpstream{t: t, replyParts: []string{toolBridgeReply}}
	ts, _ := newTestServer(t, up, goodAccount(), func(c *config.Config) { c.Facade.PlatformNotice = false })
	code, out := postClientTool(t, ts.URL, "responses", clientToolBody("responses", false))
	if code != 200 {
		t.Fatal(out)
	}
	sys, _ := requireSystemUser(t, upstreamInput(t, up, 0))
	if strings.Contains(sys, "gateway_notice") {
		t.Fatal("disabled platform notice was injected")
	}
}
