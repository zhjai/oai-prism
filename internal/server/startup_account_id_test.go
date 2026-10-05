package server

import (
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/oai-prism/oaiprism/internal/config"
)

func TestStaticAccountsWithoutIDsSurviveRestartAndDeletion(t *testing.T) {
	cfg := config.Default()
	cfg.Upstream.BaseURL = "http://127.0.0.1:1"
	cfg.Creds.File = filepath.Join(t.TempDir(), "accounts.json")
	cfg.Creds.AutoRefresh = false
	cfg.Pool.HealthCheck = false
	cfg.Creds.Accounts = []config.AccountConfig{{AccessToken: "synthetic-a"}, {AccessToken: "synthetic-b"}}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	s, err := New(cfg, logger)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if s.pool.Size() != 2 || s.pool.Get("acct-1") == nil || s.pool.Get("acct-2") == nil {
		t.Fatal("missing stable implicit account IDs")
	}
	if cfg.Creds.Accounts[0].ID != "" || cfg.Creds.Accounts[1].ID != "" {
		t.Fatal("startup mutated caller configuration")
	}
	if err := s.sqlite.DeleteAccount("acct-1"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := New(cfg, logger)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	if restarted.pool.Size() != 1 || restarted.pool.Get("acct-1") != nil || restarted.pool.Get("acct-2") == nil {
		t.Fatal("restart restored deleted implicit account or lost surviving account")
	}
}
