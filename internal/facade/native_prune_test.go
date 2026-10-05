package facade

import (
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"
)

type pruningNativeStore struct {
	memNativeStore
	cutoff time.Time
	err    error
}

func (s *pruningNativeStore) PruneNativeBindings(since time.Time) error {
	s.cutoff = since
	return s.err
}

func TestNativeStoreRuntimePruning(t *testing.T) {
	now := time.Now()
	for _, failure := range []error{nil, errors.New("storage unavailable")} {
		st := &pruningNativeStore{err: failure}
		r := &Runner{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
		r.UseNativeStore(st)
		r.pruneNativeBindings(now)
		if want := now.Add(-nativeBindingTTL); !st.cutoff.Equal(want) {
			t.Fatalf("prune cutoff = %v, want %v", st.cutoff, want)
		}
	}
	// Existing stores can omit pruning, including the in-memory-only default.
	for _, st := range []NativeStore{nil, &memNativeStore{}} {
		r := &Runner{nativeStore: st}
		r.pruneNativeBindings(now)
	}
}

func TestNativeStoreInstallationDuringCleanup(t *testing.T) {
	r := &Runner{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			r.UseNativeStore(&memNativeStore{})
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			r.pruneNativeBindings(time.Now())
		}
	}()
	wg.Wait()
}
