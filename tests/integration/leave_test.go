package integration

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/ykhdr/hubfuse/internal/common"
	"github.com/ykhdr/hubfuse/internal/hub/hubtest"
	pb "github.com/ykhdr/hubfuse/proto"
)

// TestLeave_RemovesDevice verifies that calling Leave via gRPC removes the
// device from the hub's device registry.
func TestLeave_RemovesDevice(t *testing.T) {
	h := hubtest.StartTestHub(t)
	unauthClient := dialNoClientCert(t, h)
	ctx := context.Background()

	deviceID := "leave-dev-" + uuid.New().String()
	nickname := "leave-alice-" + uuid.New().String()

	// Join.
	tok, _, err := h.Registry.IssueJoinToken(ctx)
	require.NoError(t, err, "IssueJoinToken")
	joinResp, err := unauthClient.Join(ctx, &pb.JoinRequest{
		DeviceId:  deviceID,
		Nickname:  nickname,
		JoinToken: tok,
	})
	require.NoError(t, err, "Join RPC")
	require.True(t, joinResp.Success, "Join failed: %s", joinResp.Error)

	// Register so the device is online.
	authedClient := dialWithClientCert(t, h, joinResp.ClientCert, joinResp.ClientKey)
	_, err = authedClient.Register(ctx, &pb.RegisterRequest{
		SshPort:         2222,
		ProtocolVersion: int32(common.ProtocolVersion),
	})
	require.NoError(t, err, "Register RPC")

	// Call Leave.
	leaveResp, err := authedClient.Leave(ctx, &pb.LeaveRequest{})
	require.NoError(t, err, "Leave RPC")
	require.True(t, leaveResp.Success, "Leave failed: %s", leaveResp.Error)

	// ListDevices must no longer include alice.
	// We need a different authenticated client to call ListDevices since alice is gone.
	// Use the hub's store directly to verify.
	_, storeErr := h.Store.GetDevice(ctx, deviceID)
	assert.Error(t, storeErr, "device should no longer exist in the store after Leave")
}

// TestJoin_LeaveRoundTrip verifies that after leaving, a device can rejoin
// with the same nickname without a "nickname already taken" error.
func TestJoin_LeaveRoundTrip(t *testing.T) {
	h := hubtest.StartTestHub(t)
	unauthClient := dialNoClientCert(t, h)
	ctx := context.Background()

	nickname := "roundtrip-alice-" + uuid.New().String()

	// First join.
	tok1, _, err := h.Registry.IssueJoinToken(ctx)
	require.NoError(t, err, "IssueJoinToken 1")
	joinResp1, err := unauthClient.Join(ctx, &pb.JoinRequest{
		DeviceId:  "roundtrip-dev1-" + uuid.New().String(),
		Nickname:  nickname,
		JoinToken: tok1,
	})
	require.NoError(t, err, "first Join RPC")
	require.True(t, joinResp1.Success, "first Join failed: %s", joinResp1.Error)

	// Leave.
	authedClient := dialWithClientCert(t, h, joinResp1.ClientCert, joinResp1.ClientKey)
	leaveResp, err := authedClient.Leave(ctx, &pb.LeaveRequest{})
	require.NoError(t, err, "Leave RPC")
	require.True(t, leaveResp.Success, "Leave failed: %s", leaveResp.Error)

	// Rejoin with the same nickname and a new device ID.
	tok2, _, err := h.Registry.IssueJoinToken(ctx)
	require.NoError(t, err, "IssueJoinToken 2")
	joinResp2, err := unauthClient.Join(ctx, &pb.JoinRequest{
		DeviceId:  "roundtrip-dev2-" + uuid.New().String(),
		Nickname:  nickname,
		JoinToken: tok2,
	})
	require.NoError(t, err, "second Join RPC")
	require.True(t, joinResp2.Success, "second Join failed: %s", joinResp2.Error)

	// The hub registry should have exactly one device with that nickname.
	devices, err := h.Store.ListAllDevices(ctx)
	require.NoError(t, err, "ListAllDevices")

	count := 0
	for _, d := range devices {
		if d.Nickname == nickname {
			count++
		}
	}
	assert.Equal(t, 1, count, "expected exactly one device with nickname %q after rejoin", nickname)
}
