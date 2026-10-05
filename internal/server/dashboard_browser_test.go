package server

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/oai-prism/oaiprism/internal/config"
)

// Opt-in browser verification against a real gateway and simulated upstream.
// OAIPRISM_BROWSER_TEST=1 PLAYWRIGHT_MODULE=/path/to/playwright/index.mjs go test ./internal/server -run TestDashboardBrowser -v
func TestDashboardBrowser(t *testing.T) {
	if os.Getenv("OAIPRISM_BROWSER_TEST") != "1" {
		t.Skip("opt-in Playwright verification")
	}
	ts, _ := newTestServer(t, &fakeUpstream{t: t}, []config.AccountConfig{
		{ID: "a", Name: "Account A", AccessToken: "test-token-a"},
		{ID: "b", Name: "Account B", AccessToken: "test-token-b"},
		{ID: "c", Name: "Account C", AccessToken: "test-token-c"},
	}, func(c *config.Config) { c.Facade.APIKeys = []string{"browser-test-admin-key"} })
	cmd := exec.Command("node", filepath.Join("..", "..", "tools", "test_dashboard_bindings.mjs"))
	cmd.Env = append(os.Environ(), "OAIPRISM_BROWSER_URL="+ts.URL)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("browser verification: %v\n%s", err, output)
	}
	t.Log(string(output))
}

func TestDashboardBrowserBootstrap(t *testing.T) {
	if os.Getenv("OAIPRISM_BROWSER_TEST") != "1" {
		t.Skip("opt-in Playwright verification")
	}
	ts, _ := newTestServer(t, &fakeUpstream{t: t}, []config.AccountConfig{
		{ID: "a", Name: "Account A", AccessToken: "test-token-a"},
	}, nil)
	cmd := exec.Command("node", filepath.Join("..", "..", "tools", "test_dashboard_bindings.mjs"))
	cmd.Env = append(os.Environ(), "OAIPRISM_BROWSER_URL="+ts.URL, "OAIPRISM_BROWSER_BOOTSTRAP=1")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("browser bootstrap verification: %v\n%s", err, output)
	}
	t.Log(string(output))
}
