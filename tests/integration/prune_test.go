package integration

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

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
	joinWatcher, err := unauth.Join(context.Background(), &pb.JoinRequest{
		DeviceId: "watcher-dev",
		Nickname: "watcher",
	})
	if err != nil || !joinWatcher.Success {
		t.Fatalf("join watcher: err=%v success=%v", err, joinWatcher.GetSuccess())
	}
	clientWatcher := dialWithClientCert(t, h, joinWatcher.ClientCert, joinWatcher.ClientKey)

	_, err = clientWatcher.Register(context.Background(), &pb.RegisterRequest{
		SshPort:         2222,
		ProtocolVersion: int32(common.ProtocolVersion),
	})
	if err != nil {
		t.Fatalf("register watcher: %v", err)
	}

	subCtx, cancelSub := context.WithCancel(context.Background())
	t.Cleanup(cancelSub)
	stream, err := clientWatcher.Subscribe(subCtx, &pb.SubscribeRequest{})
	if err != nil {
		t.Fatalf("subscribe watcher: %v", err)
	}
	if ev, err := stream.Recv(); err != nil || ev.GetSubscribeReady() == nil {
		t.Fatalf("subscribe ready: %v payload=%T", err, ev.GetPayload())
	}

	// Join stale device; it stays offline and stale.
	joinStale, err := unauth.Join(context.Background(), &pb.JoinRequest{
		DeviceId: "stale-dev",
		Nickname: "stale",
	})
	if err != nil || !joinStale.Success {
		t.Fatalf("join stale: err=%v success=%v", err, joinStale.GetSuccess())
	}
	// Join now leaves devices in StatusRegistered; pruning only considers
	// StatusOffline, so demote stale-dev explicitly.
	if err := h.Store.UpdateDeviceStatus(context.Background(), "stale-dev", store.StatusOffline, "", 0); err != nil {
		t.Fatalf("UpdateDeviceStatus stale-dev: %v", err)
	}

	// Set up a stubbed mounter that records unmounts.
	mtLogger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	mounter := agent.NewMounter(filepath.Join(t.TempDir(), "id_ed25519"), mtLogger)
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
	if err := mounter.Mount(context.Background(), mc, "127.0.0.1", 2222); err != nil {
		t.Fatalf("pre-mount: %v", err)
	}

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

	if mounter.IsActive("stale", "docs") {
		t.Fatal("mount still active after DeviceRemoved")
	}

	if _, err := h.Store.GetDevice(context.Background(), "stale-dev"); err == nil {
		t.Fatal("stale device still present after pruning")
	}
}
