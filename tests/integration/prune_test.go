package integration

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/ykhdr/hubfuse/internal/agent"
	agentconfig "github.com/ykhdr/hubfuse/internal/agent/config"
	"github.com/ykhdr/hubfuse/internal/common"
	"github.com/ykhdr/hubfuse/internal/hub"
	"github.com/ykhdr/hubfuse/internal/hub/hubtest"
	"github.com/ykhdr/hubfuse/internal/hub/store"
	pb "github.com/ykhdr/hubfuse/proto"
)

// TestIntegration_PruneDeviceBroadcastsRemovalAndUnmount ensures a pruned device
// triggers DeviceRemoved and the agent unmounts shares.
func TestIntegration_PruneDeviceBroadcastsRemovalAndUnmount(t *testing.T) {
	h := hubtest.StartTestHub(t)

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	monCtx, cancelMon := context.WithCancel(context.Background())
	t.Cleanup(cancelMon)
	monitor := hub.NewHeartbeatMonitor(h.Registry, h.Store, 0, 20*time.Millisecond, logger)
	go monitor.Start(monCtx)

	unauth := dialNoClientCert(t, h)

	// Register watcher device.
	watchTok, _, err := h.Registry.IssueJoinToken(context.Background())
	require.NoError(t, err, "IssueJoinToken watcher")
	joinWatcher, err := unauth.Join(context.Background(), &pb.JoinRequest{
		DeviceId:  "watcher-dev",
		Nickname:  "watcher",
		JoinToken: watchTok,
	})
	require.NoError(t, err, "join watcher: err")
	require.True(t, joinWatcher.GetSuccess(), "join watcher: success=false")
	clientWatcher := dialWithClientCert(t, h, joinWatcher.ClientCert, joinWatcher.ClientKey)

	_, err = clientWatcher.Register(context.Background(), &pb.RegisterRequest{
		SshPort:         2222,
		ProtocolVersion: int32(common.ProtocolVersion),
	})
	require.NoError(t, err, "register watcher")

	subCtx, cancelSub := context.WithCancel(context.Background())
	t.Cleanup(cancelSub)
	stream, err := clientWatcher.Subscribe(subCtx, &pb.SubscribeRequest{})
	require.NoError(t, err, "subscribe watcher")
	ev, err := stream.Recv()
	require.NoError(t, err, "subscribe ready: recv error")
	require.NotNil(t, ev.GetSubscribeReady(), "subscribe ready: unexpected payload %T", ev.GetPayload())

	// Join stale device; it stays offline and stale.
	staleTok, _, err := h.Registry.IssueJoinToken(context.Background())
	require.NoError(t, err, "IssueJoinToken stale")
	joinStale, err := unauth.Join(context.Background(), &pb.JoinRequest{
		DeviceId:  "stale-dev",
		Nickname:  "stale",
		JoinToken: staleTok,
	})
	require.NoError(t, err, "join stale: err")
	require.True(t, joinStale.GetSuccess(), "join stale: success=false")
	// Join now leaves devices in StatusRegistered; pruning only considers
	// StatusOffline, so demote stale-dev explicitly.
	require.NoError(t,
		h.Store.UpdateDeviceStatus(context.Background(), "stale-dev", store.StatusOffline, "", 0),
		"UpdateDeviceStatus stale-dev",
	)

	// Set up a stubbed mounter that records unmounts.
	mtLogger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	agentDir := t.TempDir()
	knownDevicesDir := filepath.Join(agentDir, common.KnownDevicesDir)
	knownHostsDir := filepath.Join(agentDir, common.KnownHostsDir)
	require.NoError(t, os.MkdirAll(knownDevicesDir, 0700), "MkdirAll known_devices")
	require.NoError(t,
		os.WriteFile(filepath.Join(knownDevicesDir, "stale-dev.pub"),
			[]byte("ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAItest\n"), 0644),
		"write stale-dev.pub")

	mounter := agent.NewMounter(filepath.Join(agentDir, "id_ed25519"), knownDevicesDir, knownHostsDir, mtLogger)
	mounter.SetExecCommandForTests(func(ctx context.Context, name string, args ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "true")
	})
	unmounted := make(chan struct{})
	mounter.SetUnmountForTests(func(string) error {
		close(unmounted)
		return nil
	})

	mountPath := filepath.Join(t.TempDir(), "mnt")
	mc := agentconfig.MountConfig{Device: "stale", Share: "docs", To: mountPath}
	require.NoError(t, mounter.Mount(context.Background(), mc, "stale-dev", "127.0.0.1", 2222), "pre-mount")

	done := make(chan struct{})
	go func() {
		for {
			ev, err := stream.Recv()
			if err != nil {
				close(done)
				return
			}
			if removed := ev.GetDeviceRemoved(); removed != nil {
				_ = mounter.UnmountDevice(removed.Nickname)
				close(done)
				return
			}
		}
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for DeviceRemoved")
	}

	select {
	case <-unmounted:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("UnmountDevice not called after DeviceRemoved")
	}

	assert.False(t, mounter.IsActive("stale", "docs"), "mount still active after DeviceRemoved")

	_, err = h.Store.GetDevice(context.Background(), "stale-dev")
	assert.Error(t, err, "stale device still present after pruning")
}
