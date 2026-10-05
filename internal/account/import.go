package account

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/oai-prism/oaiprism/internal/config"
	"github.com/oai-prism/oaiprism/internal/creds"
)

// ResolveImportID updates the same identity while keeping distinct imports apart.
func ResolveImportID(a config.AccountConfig, c *creds.Credential, existing []config.AccountConfig) (string, error) {
	if id := strings.TrimSpace(a.ID); id != "" {
		return id, nil
	}
	identity := strings.ToLower(strings.TrimSpace(c.Email))
	if identity != "" {
		for _, current := range existing {
			if strings.EqualFold(strings.TrimSpace(current.Email), identity) {
				return current.ID, nil
			}
		}
		key := sha256.Sum256([]byte(identity))
		return "acct-" + hex.EncodeToString(key[:8]), nil
	}
	var random [12]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", fmt.Errorf("生成账号 ID 失败: %w", err)
	}
	return "acct-" + hex.EncodeToString(random[:]), nil
}
