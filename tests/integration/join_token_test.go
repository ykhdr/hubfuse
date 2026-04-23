package integration

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/ykhdr/hubfuse/internal/hub/hubtest"
	"github.com/ykhdr/hubfuse/internal/hub/store"
	pb "github.com/ykhdr/hubfuse/proto"
)

// TestIntegration_Join_RequiresToken verifies that Join with an empty token
// fails with success=false and an error mentioning "invalid join token".
func TestIntegration_Join_RequiresToken(t *testing.T) {
	h := hubtest.StartTestHub(t)
	client := dialNoClientCert(t, h)

	resp, err := client.Join(context.Background(), &pb.JoinRequest{
		DeviceId:  "tok-req-dev-" + uuid.New().String(),
		Nickname:  "tok-req-nick-" + uuid.New().String(),
		JoinToken: "",
	})
	if err != nil {
		t.Fatalf("Join RPC transport error: %v", err)
	}
	if resp.Success {
		t.Fatal("expected Join to fail with empty token, got success=true")
	}
	const want = "invalid join token"
	if resp.Error == "" {
		t.Fatal("expected non-empty error message")
	}
	if !containsSubstring(resp.Error, want) {
		t.Errorf("resp.Error = %q, want substring %q", resp.Error, want)
	}
}

// TestIntegration_Join_WrongToken verifies that Join with a well-formed but
// never-issued token fails with success=false and the expected error substring.
func TestIntegration_Join_WrongToken(t *testing.T) {
	h := hubtest.StartTestHub(t)
	client := dialNoClientCert(t, h)

	resp, err := client.Join(context.Background(), &pb.JoinRequest{
		DeviceId:  "tok-wrong-dev-" + uuid.New().String(),
		Nickname:  "tok-wrong-nick-" + uuid.New().String(),
		JoinToken: "HUB-XXX-YYY",
	})
	if err != nil {
		t.Fatalf("Join RPC transport error: %v", err)
	}
	if resp.Success {
		t.Fatal("expected Join to fail with unrecognised token, got success=true")
	}
	const want = "invalid join token"
	if !containsSubstring(resp.Error, want) {
		t.Errorf("resp.Error = %q, want substring %q", resp.Error, want)
	}
}

// TestIntegration_Join_TokenIsSingleUse verifies that a token issued via
// IssueJoinToken can only be used once. The second Join with the same token
// must fail.
func TestIntegration_Join_TokenIsSingleUse(t *testing.T) {
	h := hubtest.StartTestHub(t)
	client := dialNoClientCert(t, h)

	ctx := context.Background()
	tok, _, err := h.Registry.IssueJoinToken(ctx)
	if err != nil {
		t.Fatalf("IssueJoinToken: %v", err)
	}

	// First Join must succeed.
	resp1, err := client.Join(ctx, &pb.JoinRequest{
		DeviceId:  "tok-single-dev1-" + uuid.New().String(),
		Nickname:  "tok-single-nick1-" + uuid.New().String(),
		JoinToken: tok,
	})
	if err != nil {
		t.Fatalf("first Join RPC transport error: %v", err)
	}
	if !resp1.Success {
		t.Fatalf("first Join failed unexpectedly: %s", resp1.Error)
	}
	if len(resp1.ClientCert) == 0 {
		t.Error("first Join: expected non-empty ClientCert")
	}

	// Second Join with the same token must fail.
	resp2, err := client.Join(ctx, &pb.JoinRequest{
		DeviceId:  "tok-single-dev2-" + uuid.New().String(),
		Nickname:  "tok-single-nick2-" + uuid.New().String(),
		JoinToken: tok,
	})
	if err != nil {
		t.Fatalf("second Join RPC transport error: %v", err)
	}
	if resp2.Success {
		t.Fatal("expected second Join with same token to fail, got success=true")
	}
	const want = "invalid join token"
	if !containsSubstring(resp2.Error, want) {
		t.Errorf("resp2.Error = %q, want substring %q", resp2.Error, want)
	}
}

// TestIntegration_Join_ExpiredToken verifies that a token whose expiry is in
// the past is rejected with success=false and an error mentioning "expired".
func TestIntegration_Join_ExpiredToken(t *testing.T) {
	h := hubtest.StartTestHub(t)
	client := dialNoClientCert(t, h)

	ctx := context.Background()

	// Insert an already-expired token directly via the store.
	expiredToken := "HUB-EXP-IRE"
	err := h.Store.CreateJoinToken(ctx, &store.JoinToken{
		Token:     expiredToken,
		ExpiresAt: time.Now().Add(-time.Minute),
		CreatedAt: time.Now().Add(-10 * time.Minute),
	})
	if err != nil {
		t.Fatalf("CreateJoinToken: %v", err)
	}

	resp, err := client.Join(ctx, &pb.JoinRequest{
		DeviceId:  "tok-exp-dev-" + uuid.New().String(),
		Nickname:  "tok-exp-nick-" + uuid.New().String(),
		JoinToken: expiredToken,
	})
	if err != nil {
		t.Fatalf("Join RPC transport error: %v", err)
	}
	if resp.Success {
		t.Fatal("expected Join with expired token to fail, got success=true")
	}
	const want = "expired"
	if !containsSubstring(resp.Error, want) {
		t.Errorf("resp.Error = %q, want substring %q", resp.Error, want)
	}
}

// containsSubstring reports whether s contains sub as a substring.
func containsSubstring(s, sub string) bool {
	return strings.Contains(s, sub)
}
