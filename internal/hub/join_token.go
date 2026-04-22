package hub

import (
	"context"
	"time"

	"github.com/ykhdr/hubfuse/internal/common"
	"github.com/ykhdr/hubfuse/internal/hub/store"
)

const defaultJoinTokenTTL = 10 * time.Minute

// IssueJoinToken creates a single-use token authorising exactly one successful
// Join. The returned code is in HUB-XXX-YYY format.
func (r *Registry) IssueJoinToken(ctx context.Context) (string, time.Time, error) {
	code := GenerateInviteCode()
	now := time.Now()
	expiresAt := now.Add(r.joinTokenTTL)
	t := &store.JoinToken{
		Token:     code,
		ExpiresAt: expiresAt,
		CreatedAt: now,
	}
	if err := r.store.CreateJoinToken(ctx, t); err != nil {
		return "", time.Time{}, err
	}
	return code, expiresAt, nil
}

// consumeJoinToken atomically claims a token. Under concurrent Joins with the
// same token, at most one caller observes a successful claim; every other
// caller receives ErrInvalidJoinToken. On a failed claim we do a best-effort
// diagnostic read to distinguish "expired" from "missing", but the claim
// itself — not the read — is what grants access.
func (r *Registry) consumeJoinToken(ctx context.Context, token string) error {
	if token == "" {
		return common.ErrInvalidJoinToken
	}
	claimed, err := r.store.ClaimJoinToken(ctx, token, time.Now())
	if err != nil {
		return err
	}
	if claimed {
		return nil
	}
	// No row was deleted — either the token never existed, already expired,
	// or was consumed by a concurrent Join. The diagnostic read is cosmetic:
	// security does not rely on it.
	t, err := r.store.GetJoinToken(ctx, token)
	if err != nil {
		return common.ErrInvalidJoinToken
	}
	if time.Now().After(t.ExpiresAt) {
		return common.ErrJoinTokenExpired
	}
	return common.ErrInvalidJoinToken
}
