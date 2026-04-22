package hub

import (
	"context"
	"time"

	"github.com/ykhdr/hubfuse/internal/common"
	"github.com/ykhdr/hubfuse/internal/hub/store"
)

const (
	defaultJoinTokenTTL  = 10 * time.Minute
	maxJoinTokenAttempts = 5
)

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

// consumeJoinToken validates the token and increments its attempt counter. It
// returns nil iff the token exists, is not expired, and has attempts < cap.
// The token is NOT deleted here — callers delete on successful Join so that
// partial failures (e.g. nickname collision) leave the token usable up to the
// attempt cap.
func (r *Registry) consumeJoinToken(ctx context.Context, token string) error {
	if token == "" {
		return common.ErrInvalidJoinToken
	}
	t, err := r.store.GetJoinToken(ctx, token)
	if err != nil {
		return common.ErrInvalidJoinToken
	}
	if time.Now().After(t.ExpiresAt) {
		return common.ErrJoinTokenExpired
	}
	if t.Attempts >= maxJoinTokenAttempts {
		return common.ErrMaxAttemptsExceeded
	}
	if err := r.store.IncrementJoinTokenAttempts(ctx, token); err != nil {
		return err
	}
	return nil
}
