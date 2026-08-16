package integration

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/ykhdr/hubfuse/internal/agent"
	"github.com/ykhdr/hubfuse/internal/hub/hubtest"
	"github.com/ykhdr/hubfuse/internal/hub/store"
	pb "github.com/ykhdr/hubfuse/proto"
)

// joinAgentClient joins deviceID/nickname to the harness hub and returns an
// agent-side HubClient authenticated with the issued certificate — the same
// wrapper the daemon uses, so these tests exercise its response validation
// rather than the raw generated stub.
func joinAgentClient(t *testing.T, h *hubtest.Harness, deviceID, nickname string) *agent.HubClient {
	t.Helper()

	token, _, err := h.Registry.IssueJoinToken(context.Background())
	require.NoError(t, err, "IssueJoinToken")

	unauth := dialNoClientCert(t, h)
	joinResp, err := unauth.Join(context.Background(), &pb.JoinRequest{
		DeviceId:  deviceID,
		Nickname:  nickname,
		JoinToken: token,
	})
	require.NoError(t, err, "Join RPC")
	require.True(t, joinResp.GetSuccess(), "Join refused: %s", joinResp.GetError())

	dir := t.TempDir()
	caPath := writeTempFile(t, dir, "ca.crt", joinResp.CaCert)
	certPath := writeTempFile(t, dir, "client.crt", joinResp.ClientCert)
	keyPath := writeTempFile(t, dir, "client.key", joinResp.ClientKey)

	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	client, err := agent.DialWithMTLS(h.Addr, caPath, certPath, keyPath, logger)
	require.NoError(t, err, "DialWithMTLS")
	t.Cleanup(func() { _ = client.Close() })

	return client
}

// TestIntegration_PrunedDeviceCannotRegister is the wire-level half of issue
// #69: the certificate still authenticates, so the RPC itself succeeds and the
// hub answers Success=false. The agent's client wrapper must turn that into an
// error carrying the hub's re-join instruction — silently treating it as
// success is what let a pruned daemon run as a ghost.
func TestIntegration_PrunedDeviceCannotRegister(t *testing.T) {
	h := hubtest.StartTestHub(t)
	ctx := context.Background()

	client := joinAgentClient(t, h, "ghost-dev", "ghost")

	// Sanity: registration works while the hub knows the device.
	_, err := client.Register(ctx, nil, 2222)
	require.NoError(t, err, "registration should succeed before the prune")

	// The hub prunes the device (retention elapsed) — everything on the agent
	// side stays exactly as it was.
	require.NoError(t, h.Store.DeleteDevice(ctx, "ghost-dev"), "DeleteDevice")

	_, err = client.Register(ctx, nil, 2222)
	require.Error(t, err, "a pruned device must not register successfully")
	assert.ErrorIs(t, err, agent.ErrHubRejected, "the refusal must be recognisable as an application-level rejection")
	assert.Contains(t, err.Error(), "not registered on the hub", "the hub's reason must reach the agent")
	assert.Contains(t, err.Error(), "hubfuse join", "the operator must be told how to recover")

	err = client.Heartbeat(ctx)
	require.Error(t, err, "a pruned device's heartbeat must not be accepted")
	assert.ErrorIs(t, err, agent.ErrHubRejected)
}

// TestIntegration_HeartbeatRecoversDemotedDevice exercises the recovery path
// over the wire: the monitor demotes a device whose heartbeats were merely
// late, and the very next heartbeat must bring it back and tell its peers — the
// share that vanished after 30s in the issue report comes back this way even
// without a full session re-establishment.
func TestIntegration_HeartbeatRecoversDemotedDevice(t *testing.T) {
	h := hubtest.StartTestHub(t)
	ctx := context.Background()

	sharer := joinAgentClient(t, h, "sharer-dev", "sharer")
	watcher := joinAgentClient(t, h, "watcher-dev", "watcher")

	_, err := sharer.Register(ctx, []*pb.Share{{Alias: "docs", Permissions: "ro"}}, 2222)
	require.NoError(t, err, "register sharer")
	_, err = watcher.Register(ctx, nil, 2223)
	require.NoError(t, err, "register watcher")

	subCtx, cancelSub := context.WithCancel(ctx)
	t.Cleanup(cancelSub)
	stream, err := watcher.Subscribe(subCtx)
	require.NoError(t, err, "subscribe watcher")
	ready, err := stream.Recv()
	require.NoError(t, err, "subscribe ready")
	require.NotNil(t, ready.GetSubscribeReady(), "expected SubscribeReady, got %T", ready.GetPayload())

	events := make(chan *pb.Event, 8)
	go func() {
		for {
			ev, recvErr := stream.Recv()
			if recvErr != nil {
				return
			}
			events <- ev
		}
	}()

	// The hub gives up on the sharer while its process is alive and connected.
	sharerDev, err := h.Store.GetDevice(ctx, "sharer-dev")
	require.NoError(t, err, "GetDevice")
	demoted, err := h.Registry.MarkOffline(ctx, sharerDev, time.Now().Add(time.Minute))
	require.NoError(t, err, "MarkOffline")
	require.True(t, demoted, "the sharer should have been demoted")

	select {
	case ev := <-events:
		require.NotNil(t, ev.GetDeviceOffline(), "expected DeviceOffline, got %T", ev.GetPayload())
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for the demotion DeviceOffline event")
	}

	require.NoError(t, sharer.Heartbeat(ctx), "the sharer's heartbeat must be accepted")

	select {
	case ev := <-events:
		online := ev.GetDeviceOnline()
		require.NotNil(t, online, "expected DeviceOnline, got %T", ev.GetPayload())
		assert.Equal(t, "sharer-dev", online.DeviceId)
		assert.Equal(t, int32(2222), online.SshPort, "peers need the endpoint to remount")
		require.Len(t, online.Shares, 1, "peers need the share list to remount")
		assert.Equal(t, "docs", online.Shares[0].Alias)
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for the recovery DeviceOnline event")
	}

	dev, err := h.Store.GetDevice(ctx, "sharer-dev")
	require.NoError(t, err, "GetDevice after recovery")
	assert.Equal(t, store.StatusOnline, dev.Status)

	// The recovery is announced once: a device already online has nothing to
	// announce, however often it beats.
	require.NoError(t, sharer.Heartbeat(ctx), "second heartbeat")
	select {
	case ev := <-events:
		t.Fatalf("recovery must be announced once, got another %T", ev.GetPayload())
	case <-time.After(300 * time.Millisecond):
	}
}

// writeTempFile writes the TLS material a HubClient dial needs to disk and
// returns the path.
func writeTempFile(t *testing.T, dir, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(path, data, 0o600), "write %s", name)
	return path
}
