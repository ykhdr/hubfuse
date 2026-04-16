package integration

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/ykhdr/hubfuse/internal/common"
	"github.com/ykhdr/hubfuse/internal/hub/hubtest"
	pb "github.com/ykhdr/hubfuse/proto"
)

// TestIntegration_Reconnect_AgentSurvivesHubRestart verifies that after a hub
// restart (same data directory → same CA and DB):
//  1. An agent that joined and registered before the restart can reconnect
//     using its existing certificate.
//  2. Pairings created before the restart still exist in the database.
func TestIntegration_Reconnect_AgentSurvivesHubRestart(t *testing.T) {
	dataDir := t.TempDir()

	h1 := hubtest.StartTestHubWithOptions(t, hubtest.Options{DataDir: dataDir})

	deviceID := "reconnect-dev-" + uuid.New().String()

	unauthClient1 := dialNoClientCert(t, h1)
	joinResp, err := unauthClient1.Join(context.Background(), &pb.JoinRequest{
		DeviceId: deviceID,
		Nickname: "reconnect-alice-" + uuid.New().String(),
	})
	if err != nil || !joinResp.Success {
		t.Fatalf("Join before restart: err=%v success=%v", err, joinResp.GetSuccess())
	}

	clientCertPEM := joinResp.ClientCert
	clientKeyPEM := joinResp.ClientKey

	authedClient1 := dialWithClientCert(t, h1, clientCertPEM, clientKeyPEM)

	regResp1, err := authedClient1.Register(context.Background(), &pb.RegisterRequest{
		SshPort:         22,
		ProtocolVersion: int32(common.ProtocolVersion),
	})
	if err != nil || !regResp1.Success {
		t.Fatalf("Register before restart: err=%v success=%v", err, regResp1.GetSuccess())
	}

	devB := "reconnect-b-" + uuid.New().String()
	nickB := "reconnect-bob-" + uuid.New().String()
	joinRespB, err := unauthClient1.Join(context.Background(), &pb.JoinRequest{
		DeviceId: devB,
		Nickname: nickB,
	})
	if err != nil || !joinRespB.Success {
		t.Fatalf("Join B before restart: err=%v success=%v", err, joinRespB.GetSuccess())
	}
	clientB1 := dialWithClientCert(t, h1, joinRespB.ClientCert, joinRespB.ClientKey)
	regRespB, err := clientB1.Register(context.Background(), &pb.RegisterRequest{
		SshPort:         22,
		ProtocolVersion: int32(common.ProtocolVersion),
	})
	if err != nil || !regRespB.Success {
		t.Fatalf("Register B before restart: err=%v", err)
	}

	pairResp, err := authedClient1.RequestPairing(context.Background(), &pb.RequestPairingRequest{
		ToDevice:  nickB,
		PublicKey: "pk-alice",
	})
	if err != nil {
		t.Fatalf("RequestPairing before restart: %v", err)
	}
	confirmResp, err := clientB1.ConfirmPairing(context.Background(), &pb.ConfirmPairingRequest{
		DeviceId:   devB,
		InviteCode: pairResp.InviteCode,
		PublicKey:  "pk-bob",
	})
	if err != nil || !confirmResp.Success {
		t.Fatalf("ConfirmPairing before restart: err=%v success=%v", err, confirmResp.GetSuccess())
	}

	h1.Stop()

	h2 := hubtest.StartTestHubWithOptions(t, hubtest.Options{DataDir: dataDir})

	authedClient2 := dialWithClientCert(t, h2, clientCertPEM, clientKeyPEM)

	regResp2, err := authedClient2.Register(context.Background(), &pb.RegisterRequest{
		SshPort:         22,
		ProtocolVersion: int32(common.ProtocolVersion),
	})
	if err != nil {
		t.Fatalf("Register after restart: %v", err)
	}
	if !regResp2.Success {
		t.Fatalf("Register after restart failed: %s", regResp2.Error)
	}

	found := false
	for _, d := range regResp2.DevicesOnline {
		if d.DeviceId == deviceID {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("device %q not in DevicesOnline after restart", deviceID)
	}

	clientB2 := dialWithClientCert(t, h2, joinRespB.ClientCert, joinRespB.ClientKey)
	regRespB2, err := clientB2.Register(context.Background(), &pb.RegisterRequest{
		SshPort:         22,
		ProtocolVersion: int32(common.ProtocolVersion),
	})
	if err != nil || !regRespB2.Success {
		t.Fatalf("Register B after restart: err=%v", err)
	}

	_, pairErr := authedClient2.RequestPairing(context.Background(), &pb.RequestPairingRequest{
		ToDevice:  nickB,
		PublicKey: "pk-alice",
	})
	if pairErr == nil {
		t.Error("expected RequestPairing to fail with ErrPairingAlreadyExists after restart, got nil")
	}
}
