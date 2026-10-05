package server

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oai-prism/oaiprism/internal/config"
)

// Opt-in check with the real client, synthetic upstream and isolated client home.
func TestCodexCLIProgress(t *testing.T) {
	if os.Getenv("OAIPRISM_CODEX_TEST") != "1" {
		t.Skip("opt-in Codex CLI protocol verification")
	}
	cli, err := exec.LookPath("codex")
	if err != nil {
		t.Fatal(err)
	}
	up := &fakeUpstream{t: t}
	handler := up.handler()
	var polls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/llm/response_with_tools_status" {
			handler.ServeHTTP(w, r)
			return
		}
		var req map[string]any
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		n := polls.Add(1)
		envelope := map[string]any{
			"status": "pending", "request_id": req["request_id"],
			"turn_state": map[string]any{"seq": n + 1},
			"codex_live_progress": map[string]any{"eventPreviews": []any{
				map[string]any{"payload_type": "agent_reasoning", "line_index": 1,
					"raw": map[string]any{"payload": map[string]any{"text": "PRISM_REASONING_CHECK"}}},
				map[string]any{"payload_type": "agent_message", "line_index": 2,
					"raw": map[string]any{"payload": map[string]any{"message": "PRISM_PROGRESS_CHECK"}}},
			}},
		}
		if n > 1 {
			envelope["status"] = "completed"
			envelope["response"] = map[string]any{"status": "success", "payload": up.payloadOutput("PRISM_FINAL_OK", true)}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(envelope)
	}))
	defer upstream.Close()
	ts, _ := newTestServer(t, up, goodAccount(), func(c *config.Config) {
		c.Upstream.BaseURL = upstream.URL
		c.Facade.APIKeys = []string{"synthetic-cli-key"}
	})
	clientHome := t.TempDir()
	clientState := filepath.Join(clientHome, "codex-state")
	if err := os.Mkdir(clientState, 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, cli, "exec", "--ephemeral", "--ignore-user-config",
		"--ignore-rules", "--skip-git-repo-check", "--sandbox", "read-only", "--json", "-C", clientHome,
		"-m", "gpt-6.1-sol", "-c", `model_provider="prism_test"`,
		"-c", `model_providers.prism_test.name="Synthetic Prism"`,
		"-c", `model_providers.prism_test.wire_api="responses"`,
		"-c", `model_providers.prism_test.requires_openai_auth=false`,
		"-c", `model_providers.prism_test.env_key="OAIPRISM_TEST_KEY"`,
		"-c", `model_providers.prism_test.base_url="`+ts.URL+`/v1"`,
		"-c", `model_reasoning_effort="low"`, "Return PRISM_FINAL_OK only.")
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + clientHome,
		"CODEX_HOME=" + clientState, "OAIPRISM_TEST_KEY=synthetic-cli-key", "NO_PROXY=127.0.0.1,localhost"}
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Codex CLI failed: %v\n%s", err, output)
	}
	counts := map[string]int{}
	completed := false
	scanner := bufio.NewScanner(strings.NewReader(string(output)))
	for scanner.Scan() {
		var event map[string]any
		if json.Unmarshal(scanner.Bytes(), &event) != nil {
			continue
		}
		if event["type"] == "turn.failed" || event["type"] == "error" {
			t.Fatalf("Codex protocol error: %s", scanner.Text())
		}
		completed = completed || event["type"] == "turn.completed"
		if event["type"] != "item.completed" {
			continue
		}
		item, _ := event["item"].(map[string]any)
		text, _ := item["text"].(string)
		counts[text]++
	}
	if !completed || counts["PRISM_PROGRESS_CHECK"] != 1 || counts["PRISM_FINAL_OK"] != 1 {
		t.Fatalf("missing or duplicated commentary/final: counts=%v completed=%v\n%s", counts, completed, output)
	}
	version, _ := exec.Command(cli, "--version").Output()
	t.Logf("%s: commentary and final each received once; isolated home and synthetic upstream", strings.TrimSpace(string(version)))
}
