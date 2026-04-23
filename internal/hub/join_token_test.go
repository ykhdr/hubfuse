package hub

import (
	"context"
	"errors"
	"regexp"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/ykhdr/hubfuse/internal/common"
	"github.com/ykhdr/hubfuse/internal/hub/store"
)

var joinTokenRe = regexp.MustCompile(`^HUB-[A-Z0-9]{3}-[A-Z0-9]{3}$`)

func TestRegistry_IssueJoinToken_FormatAndPersisted(t *testing.T) {
	r := newTestRegistry(t)
	ctx := context.Background()

	token, expiresAt, err := r.IssueJoinToken(ctx)
	require.NoError(t, err, "IssueJoinToken")
	assert.True(t, joinTokenRe.MatchString(token), "token %q does not match HUB-XXX-YYY format", token)
	assert.True(t, expiresAt.After(time.Now()), "expiresAt should be in the future")

	got, err := r.store.GetJoinToken(ctx, token)
	require.NoError(t, err, "GetJoinToken")
	assert.Equal(t, token, got.Token)
}

func TestRegistry_Join_ConsumesToken(t *testing.T) {
	r := newTestRegistry(t)
	ctx := context.Background()

	token, _, err := r.IssueJoinToken(ctx)
	require.NoError(t, err, "IssueJoinToken")

	// First Join with the token should succeed.
	_, _, _, err = r.Join(ctx, "dev-1", "alice", "10.0.0.1", token)
	require.NoError(t, err, "first Join")

	// Second Join reusing the same token should fail — token was deleted.
	_, _, _, err = r.Join(ctx, "dev-2", "bob", "10.0.0.2", token)
	require.Error(t, err, "expected error on second Join with same token")
	assert.True(t, errors.Is(err, common.ErrInvalidJoinToken), "want ErrInvalidJoinToken, got %v", err)
}

func TestRegistry_Join_EmptyToken(t *testing.T) {
	r := newTestRegistry(t)
	ctx := context.Background()

	_, _, _, err := r.Join(ctx, "dev-1", "alice", "", "")
	require.Error(t, err, "expected error for empty token")
	assert.True(t, errors.Is(err, common.ErrInvalidJoinToken), "want ErrInvalidJoinToken, got %v", err)
}

func TestRegistry_Join_ExpiredToken(t *testing.T) {
	r := newTestRegistry(t)
	ctx := context.Background()

	expired := &store.JoinToken{
		Token:     "HUB-EXP-IRD",
		ExpiresAt: time.Now().Add(-time.Minute),
		CreatedAt: time.Now().Add(-2 * time.Minute),
	}
	err := r.store.CreateJoinToken(ctx, expired)
	require.NoError(t, err, "CreateJoinToken (expired)")

	_, _, _, err = r.Join(ctx, "dev-1", "alice", "", "HUB-EXP-IRD")
	require.Error(t, err, "expected error for expired token")
	assert.True(t, errors.Is(err, common.ErrJoinTokenExpired), "want ErrJoinTokenExpired, got %v", err)
}

// TestRegistry_Join_ConcurrentSameToken is the regression test for the race
// Copilot flagged: multiple Joins holding the same token must produce exactly
// one success — the atomic ClaimJoinToken is what makes that true.
func TestRegistry_Join_ConcurrentSameToken(t *testing.T) {
	r := newTestRegistry(t)
	ctx := context.Background()

	token, _, err := r.IssueJoinToken(ctx)
	require.NoError(t, err, "IssueJoinToken")

	const goroutines = 16
	var (
		wg       sync.WaitGroup
		success  atomic.Int32
		rejected atomic.Int32
	)
	start := make(chan struct{})
	for i := 0; i < goroutines; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			deviceID := "race-dev-" + string(rune('A'+i))
			nickname := "race-nick-" + string(rune('A'+i))
			_, _, _, err := r.Join(ctx, deviceID, nickname, "", token)
			switch {
			case err == nil:
				success.Add(1)
			case errors.Is(err, common.ErrInvalidJoinToken):
				rejected.Add(1)
			default:
				t.Errorf("unexpected error from concurrent Join: %v", err)
			}
		}()
	}
	close(start)
	wg.Wait()

	assert.Equal(t, int32(1), success.Load(), "exactly one concurrent Join must succeed")
	assert.Equal(t, int32(goroutines-1), rejected.Load(), "every other Join must be rejected as invalid")
}
