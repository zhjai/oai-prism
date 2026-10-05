package account

import (
	"context"
	"testing"
	"time"

	"github.com/oai-prism/oaiprism/internal/config"
)

func TestPool_ReloadKeepsLiveConcurrencyAndCooldown(t *testing.T) {
	cfgs := []config.AccountConfig{{ID: "a", AccessToken: "token", MaxConcurrency: 1}}
	p := testPool(t, "least_inflight", cfgs...)
	lease, err := p.AcquirePinned(context.Background(), "a")
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	if err := p.Build(cfgs); err != nil {
		t.Fatal(err)
	}
	if p.Get("a").Inflight() != 1 {
		t.Fatal("reload reset active leases")
	}
	if extra, err := p.AcquirePinned(context.Background(), "a"); err == nil {
		extra.Release()
		t.Fatal("reload allowed requests above the concurrency limit")
	}
	lease.Release()
	if p.Get("a").Inflight() != 0 {
		t.Fatal("old lease did not release current concurrency")
	}
	p.Get("a").Cooldown(time.Now(), time.Minute)
	p.Get("a").MarkAuthFailed()
	if err := p.Build(cfgs); err != nil {
		t.Fatal(err)
	}
	if p.Get("a").CooldownRemaining(time.Now()) <= 0 || !p.Get("a").AuthFailed() {
		t.Fatal("reload reset cooldown/auth failure")
	}
}

func TestPool_OldSnapshotHonorsNewConcurrencyAndRate(t *testing.T) {
	cfg := config.AccountConfig{ID: "a", AccessToken: "token", MaxConcurrency: 3, RatePerSecond: 0.001, RateBurst: 1}
	p := testPool(t, "round_robin", cfg)
	defer p.Close()
	old := p.Get("a")
	if !old.Acquire(time.Now()) {
		t.Fatal("first lease rejected")
	}
	old.Release()
	cfg.MaxConcurrency = 1
	if err := p.Build([]config.AccountConfig{cfg}); err != nil {
		t.Fatal(err)
	}
	if old.MaxConcurrency() != 1 {
		t.Fatal("old snapshot retained stale concurrency limit")
	}
	if p.Get("a").Acquire(time.Now()) {
		t.Fatal("reload replenished rate bucket")
	}
}
