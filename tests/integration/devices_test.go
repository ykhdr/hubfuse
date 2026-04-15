package integration

import (
	"context"
	"testing"

	"github.com/ykhdr/hubfuse/internal/common"
	pb "github.com/ykhdr/hubfuse/proto"
)

func TestListDevices_AllStatuses(t *testing.T) {
	h := startTestHub(t)

	unauthClient := dialNoClientCert(t, h)

	// Join two devices.
	join1, err := unauthClient.Join(context.Background(), &pb.JoinRequest{
		DeviceId: "dev-ld-1",
		Nickname: "ld-alice",
	})
	if err != nil || !join1.Success {
		t.Fatalf("Join dev1: err=%v", err)
	}

	join2, err := unauthClient.Join(context.Background(), &pb.JoinRequest{
		DeviceId: "dev-ld-2",
		Nickname: "ld-bob",
	})
	if err != nil || !join2.Success {
		t.Fatalf("Join dev2: err=%v", err)
	}

	// Register only device 1.
	client1 := dialWithClientCert(t, h, join1.ClientCert, join1.ClientKey)
	_, err = client1.Register(context.Background(), &pb.RegisterRequest{
		SshPort:         2222,
		ProtocolVersion: int32(common.ProtocolVersion),
	})
	if err != nil {
		t.Fatalf("Register dev1: %v", err)
	}

	// ListDevices should return both.
	resp, err := client1.ListDevices(context.Background(), &pb.ListDevicesRequest{})
	if err != nil {
		t.Fatalf("ListDevices: %v", err)
	}

	if len(resp.Devices) != 2 {
		t.Fatalf("expected 2 devices, got %d", len(resp.Devices))
	}

	statusMap := map[string]string{}
	for _, d := range resp.Devices {
		statusMap[d.Nickname] = d.Status
	}
	if statusMap["ld-alice"] != "online" {
		t.Errorf("alice status = %q, want online", statusMap["ld-alice"])
	}
	if statusMap["ld-bob"] != "offline" {
		t.Errorf("bob status = %q, want offline", statusMap["ld-bob"])
	}
}
