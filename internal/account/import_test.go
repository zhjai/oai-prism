package account

import (
	"github.com/oai-prism/oaiprism/internal/config"
	"github.com/oai-prism/oaiprism/internal/creds"
	"testing"
)

func TestResolveImportID_DistinctAndExistingIdentities(t *testing.T) {
	existing := []config.AccountConfig{{ID: "main", Email: "first@example.invalid"}}
	first, err := ResolveImportID(config.AccountConfig{}, &creds.Credential{Email: "FIRST@example.invalid"}, existing)
	if err != nil || first != "main" {
		t.Fatalf("identity update: %q %v", first, err)
	}
	second, err := ResolveImportID(config.AccountConfig{}, &creds.Credential{Email: "second@example.invalid"}, existing)
	if err != nil || second == first {
		t.Fatalf("different identity collided: %q %v", second, err)
	}
	one, err := ResolveImportID(config.AccountConfig{}, &creds.Credential{}, existing)
	if err != nil {
		t.Fatal(err)
	}
	two, err := ResolveImportID(config.AccountConfig{}, &creds.Credential{}, existing)
	if err != nil || one == two {
		t.Fatal("unknown identity overwrote a prior import")
	}
}
