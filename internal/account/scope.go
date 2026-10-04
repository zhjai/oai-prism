package account

import "context"

// Scope restricts scheduling to a key's bound accounts. Restricted with no IDs
// deliberately denies all accounts (for example after deleting the last binding).
type Scope struct {
	Restricted bool
	AccountIDs []string
}

type scopeKey struct{}

func WithScope(ctx context.Context, scope Scope) context.Context {
	scope.AccountIDs = append([]string(nil), scope.AccountIDs...)
	return context.WithValue(ctx, scopeKey{}, scope)
}

func ScopeFromContext(ctx context.Context) Scope {
	scope, _ := ctx.Value(scopeKey{}).(Scope)
	return scope
}

func Allowed(ctx context.Context, id string) bool {
	scope := ScopeFromContext(ctx)
	if !scope.Restricted {
		return true
	}
	for _, allowed := range scope.AccountIDs {
		if allowed == id {
			return true
		}
	}
	return false
}
