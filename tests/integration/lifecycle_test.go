package integration

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/ykhdr/hubfuse/internal/common"
	"github.com/ykhdr/hubfuse/internal/hub/hubtest"
	pb "github.com/ykhdr/hubfuse/proto"
)

// recvEvent reads the next event from stream with a timeout. It fatals if no
// event arrives within the deadline.
func recvEvent(t *testing.T, stream pb.HubFuse_SubscribeClient, timeout time.Duration) *pb.Event {
	t.Helper()

	ch := make(chan *pb.Event, 1)
	go func() {
		ev, err := stream.Recv()
		if err != nil {
			return
		}
		ch <- ev
	}()

	select {
	case ev := <-ch:
		return ev
	case <-time.After(timeout):
		t.Fatalf("recvEvent: timed out after %v waiting for event", timeout)
		return nil
	}
}

// TestIntegration_Lifecycle_DeviceOnlineOfflineAndSharesUpdate exercises the
// full device lifecycle as observed by a subscribed peer:
//  1. A joins and registers with shares.
//  2. B joins, subscribes, then registers.
//  3. B receives DeviceOnline for A (because A was already online when B subscribed
//     and then B registers — but the hub only sends DeviceOnline on Register, not
//     retroactively, so we have A register after B subscribes).
//  4. A updates shares → B receives SharesUpdated.
//  5. A deregisters → B receives DeviceOffline.
func TestIntegration_Lifecycle_DeviceOnlineOfflineAndSharesUpdate(t *testing.T) {
	h := hubtest.StartTestHub(t)

	devA := "lc-a-" + uuid.New().String()
	devB := "lc-b-" + uuid.New().String()

	// B joins first so it can subscribe before A registers.
	unauthClient := dialNoClientCert(t, h)

	joinRespB, err := unauthClient.Join(context.Background(), &pb.JoinRequest{
		DeviceId: devB,
		Nickname: "lc-bob-" + uuid.New().String(),
	})
	if err != nil || !joinRespB.Success {
		t.Fatalf("Join B: err=%v success=%v", err, joinRespB.GetSuccess())
	}
	clientB := dialWithClientCert(t, h, joinRespB.ClientCert, joinRespB.ClientKey)

	regRespB, err := clientB.Register(context.Background(), &pb.RegisterRequest{
		SshPort:         22,
		ProtocolVersion: int32(common.ProtocolVersion),
	})
	if err != nil || !regRespB.Success {
		t.Fatalf("Register B: err=%v success=%v", err, regRespB.GetSuccess())
	}

	// B subscribes.
	subCtxB, cancelB := context.WithCancel(context.Background())
	t.Cleanup(cancelB)
	streamB, err := clientB.Subscribe(subCtxB, &pb.SubscribeRequest{})
	if err != nil {
		t.Fatalf("Subscribe B: %v", err)
	}
	// Wait for SubscribeReady so the server has registered B's event channel.
	if ev, err := streamB.Recv(); err != nil {
		t.Fatalf("Subscribe B ready: %v", err)
	} else if ev.GetSubscribeReady() == nil {
		t.Fatalf("Subscribe B: expected SubscribeReady, got %T", ev.GetPayload())
	}

	// A joins and registers with an initial share.
	joinRespA, err := unauthClient.Join(context.Background(), &pb.JoinRequest{
		DeviceId: devA,
		Nickname: "lc-alice-" + uuid.New().String(),
	})
	if err != nil || !joinRespA.Success {
		t.Fatalf("Join A: err=%v success=%v", err, joinRespA.GetSuccess())
	}
	clientA := dialWithClientCert(t, h, joinRespA.ClientCert, joinRespA.ClientKey)

	initialShares := []*pb.Share{
		{Alias: "docs", Permissions: "ro", AllowedDevices: []string{"all"}},
	}

	regRespA, err := clientA.Register(context.Background(), &pb.RegisterRequest{
		SshPort:         22,
		ProtocolVersion: int32(common.ProtocolVersion),
		Shares:          initialShares,
	})
	if err != nil || !regRespA.Success {
		t.Fatalf("Register A: err=%v success=%v", err, regRespA.GetSuccess())
	}

	// B should receive DeviceOnline for A.
	ev1 := recvEvent(t, streamB, 5*time.Second)
	if ev1.GetDeviceOnline() == nil {
		t.Fatalf("B expected DeviceOnline event, got %T", ev1.GetPayload())
	}
	if ev1.GetDeviceOnline().DeviceId != devA {
		t.Errorf("DeviceOnline.DeviceId = %q, want %q", ev1.GetDeviceOnline().DeviceId, devA)
	}

	// A updates shares → B receives SharesUpdated.
	updatedShares := []*pb.Share{
		{Alias: "music", Permissions: "rw", AllowedDevices: []string{devB}},
	}
	updateResp, err := clientA.UpdateShares(context.Background(), &pb.UpdateSharesRequest{
		Shares: updatedShares,
	})
	if err != nil {
		t.Fatalf("UpdateShares: %v", err)
	}
	if !updateResp.Success {
		t.Fatal("UpdateShares: success=false")
	}

	ev2 := recvEvent(t, streamB, 5*time.Second)
	if ev2.GetSharesUpdated() == nil {
		t.Fatalf("B expected SharesUpdated event, got %T", ev2.GetPayload())
	}
	if ev2.GetSharesUpdated().DeviceId != devA {
		t.Errorf("SharesUpdated.DeviceId = %q, want %q", ev2.GetSharesUpdated().DeviceId, devA)
	}
	if len(ev2.GetSharesUpdated().Shares) == 0 {
		t.Error("SharesUpdated: Shares is empty")
	}

	// A deregisters → B receives DeviceOffline.
	deregResp, err := clientA.Deregister(context.Background(), &pb.DeregisterRequest{})
	if err != nil {
		t.Fatalf("Deregister A: %v", err)
	}
	if !deregResp.Success {
		t.Fatal("Deregister A: success=false")
	}

	ev3 := recvEvent(t, streamB, 5*time.Second)
	if ev3.GetDeviceOffline() == nil {
		t.Fatalf("B expected DeviceOffline event, got %T", ev3.GetPayload())
	}
	if ev3.GetDeviceOffline().DeviceId != devA {
		t.Errorf("DeviceOffline.DeviceId = %q, want %q", ev3.GetDeviceOffline().DeviceId, devA)
	}
}
