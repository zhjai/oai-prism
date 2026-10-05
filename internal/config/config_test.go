package config

import (
	"os"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestShippedExampleParses(t *testing.T) {
	b, err := os.ReadFile("../../configs/config.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	cfg := Default()
	if err := yaml.Unmarshal(b, cfg); err != nil {
		t.Fatalf("shipped example cannot be parsed: %v", err)
	}
	if cfg.RequestLogs.MaxAge != 0 {
		t.Fatal("example must retain existing request logs by default")
	}
}

func TestApplyEnv_MergesCredentialsForOneAccount(t *testing.T) {
	t.Setenv("OAI_PRISM_COOKIE", "prism_session_token=synthetic")
	t.Setenv("OAI_PRISM_ACCESS_TOKEN", "access")
	t.Setenv("OAI_PRISM_REFRESH_TOKEN", "refresh")
	cfg := Default()
	applyEnv(cfg)
	if len(cfg.Creds.Accounts) != 1 {
		t.Fatal("environment credentials created duplicate accounts")
	}
	a := cfg.Creds.Accounts[0]
	if a.AccessToken != "access" || a.RefreshToken != "refresh" || a.Cookies == "" {
		t.Fatal("environment fields were lost")
	}
}

func TestServerAddr_IPv6(t *testing.T) {
	for _, host := range []string{"::1", "[::1]"} {
		if got := (ServerConfig{Host: host, Port: 8787}).Addr(); got != "[::1]:8787" {
			t.Fatalf("invalid IPv6 address %q", got)
		}
	}
}

func TestApplyEnv_Aliases(t *testing.T) {
	os.Setenv("PORT", "19090")
	os.Setenv("HOST", "127.0.0.2")
	os.Setenv("PRISM_COOKIE", "cookie-from-env")
	os.Setenv("PROXY_API_KEY", "sk-proxy-alias")
	os.Setenv("CORS_ORIGIN", "https://example.com")
	os.Setenv("PRISM_MODEL", "gpt-5.6-sol-custom")
	os.Setenv("PRISM_BASE_URL", "https://prism.custom.domain")

	defer func() {
		os.Unsetenv("PORT")
		os.Unsetenv("HOST")
		os.Unsetenv("PRISM_COOKIE")
		os.Unsetenv("PROXY_API_KEY")
		os.Unsetenv("CORS_ORIGIN")
		os.Unsetenv("PRISM_MODEL")
		os.Unsetenv("PRISM_BASE_URL")
	}()

	cfg := Default()
	applyEnv(cfg)

	if cfg.Server.Port != 19090 {
		t.Errorf("PORT 别名未生效: %d", cfg.Server.Port)
	}
	if cfg.Server.Host != "127.0.0.2" {
		t.Errorf("HOST 别名未生效: %q", cfg.Server.Host)
	}
	if cfg.Upstream.BaseURL != "https://prism.custom.domain" {
		t.Errorf("PRISM_BASE_URL 别名未生效: %q", cfg.Upstream.BaseURL)
	}
	if len(cfg.Facade.APIKeys) != 1 || cfg.Facade.APIKeys[0] != "sk-proxy-alias" {
		t.Errorf("PROXY_API_KEY 别名未生效: %+v", cfg.Facade.APIKeys)
	}
	if cfg.Server.CORSOrigin != "https://example.com" {
		t.Errorf("CORS_ORIGIN 别名未生效: %q", cfg.Server.CORSOrigin)
	}
	if cfg.Facade.DefaultModel != "gpt-5.6-sol-custom" {
		t.Errorf("PRISM_MODEL 别名未生效: %q", cfg.Facade.DefaultModel)
	}
	if len(cfg.Creds.Accounts) == 0 || cfg.Creds.Accounts[0].Cookies != "cookie-from-env" {
		t.Errorf("PRISM_COOKIE 别名未生效: %+v", cfg.Creds.Accounts)
	}
}
