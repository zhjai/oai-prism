package server

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oai-prism/oaiprism/internal/config"
)

func TestAutomaticRefreshRequiresDurableStorage(t *testing.T) {
	settings := config.Default()
	settings.Creds.File = filepath.Join(t.TempDir(), "accounts.json")
	settings.Upstream.BaseURL = "http://127.0.0.1:1"
	if err := os.Mkdir(filepath.Join(filepath.Dir(settings.Creds.File), "accounts.db"), 0o700); err != nil {
		t.Fatal(err)
	}
	server, err := New(settings, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if server != nil {
		_ = server.Close()
		t.Fatal("automatic refresh started without a durable credential store")
	}
	if err == nil || !strings.Contains(err.Error(), "无法安全保存自动刷新凭据") {
		t.Fatalf("expected explicit durable refresh failure, got %v", err)
	}
}
