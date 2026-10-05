package account

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/oai-prism/oaiprism/internal/config"
)

func TestPoolRateLimitedSelectionTriesOtherAccounts(t *testing.T) {
	for _, strategy := range []string{"least_inflight", "sticky_hash", "weighted", "round_robin", "random"} {
		t.Run(strategy, func(t *testing.T) {
			p := testPool(t, strategy,
				config.AccountConfig{ID: "a", AccessToken: "a", RatePerSecond: 0.001, RateBurst: 1},
				config.AccountConfig{ID: "b", AccessToken: "b"},
			)
			defer p.Close()
			if !p.Get("a").Acquire(time.Now()) {
				t.Fatal("initial rate token unavailable")
			}
			p.Get("a").Release()
			for i := 0; i < 100; i++ {
				lease, err := p.Acquire(context.Background(), "")
				if err != nil {
					t.Fatalf("available account b was skipped: %v", err)
				}
				if lease.Account.ID != "b" {
					t.Fatalf("rate-limited account selected: %s", lease.Account.ID)
				}
				lease.Release()
			}
			ctx := WithScope(context.Background(), Scope{Restricted: true, AccountIDs: []string{"a"}})
			if lease, err := p.Acquire(ctx, ""); !errors.Is(err, ErrNoAccount) {
				if lease != nil {
					lease.Release()
				}
				t.Fatalf("rate fallback escaped scope: %v", err)
			}
			if lease, err := p.AcquirePinned(context.Background(), "a"); err == nil {
				lease.Release()
				t.Fatal("pinned request fell back to another account")
			}
		})
	}
}
