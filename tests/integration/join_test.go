package integration

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"testing"

	"github.com/google/uuid"
	"github.com/ykhdr/hubfuse/internal/common"
	"github.com/ykhdr/hubfuse/internal/hub/hubtest"
	pb "github.com/ykhdr/hubfuse/proto"
)

// TestIntegration_Join_Success verifies a basic Join flow:
// the response carries success=true and non-empty cert/key/ca_cert.
func TestIntegration_Join_Success(t *testing.T) {
	h := hubtest.StartTestHub(t)
	client := dialNoClientCert(t, h)

	resp, err := client.Join(context.Background(), &pb.JoinRequest{
		DeviceId: "join-dev-" + uuid.New().String(),
		Nickname: "join-alice-" + uuid.New().String(),
	})
	if err != nil {
		t.Fatalf("Join RPC: %v", err)
	}
	if !resp.Success {
		t.Fatalf("Join failed: %s", resp.Error)
	}
	if len(resp.ClientCert) == 0 {
		t.Error("Join: ClientCert is empty")
	}
	if len(resp.ClientKey) == 0 {
		t.Error("Join: ClientKey is empty")
	}
	if len(resp.CaCert) == 0 {
		t.Error("Join: CaCert is empty")
	}
}

// TestIntegration_Join_CertHasDeviceIDInCN verifies that the signed client
// certificate returned by Join has the device_id as the Common Name.
func TestIntegration_Join_CertHasDeviceIDInCN(t *testing.T) {
	h := hubtest.StartTestHub(t)
	client := dialNoClientCert(t, h)

	deviceID := "cn-dev-" + uuid.New().String()

	resp, err := client.Join(context.Background(), &pb.JoinRequest{
		DeviceId: deviceID,
		Nickname: "cn-alice-" + uuid.New().String(),
	})
	if err != nil {
		t.Fatalf("Join RPC: %v", err)
	}
	if !resp.Success {
		t.Fatalf("Join failed: %s", resp.Error)
	}

	block, _ := pem.Decode(resp.ClientCert)
	if block == nil {
		t.Fatal("Join: cannot decode ClientCert PEM")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("Join: parse ClientCert: %v", err)
	}
	if cert.Subject.CommonName != deviceID {
		t.Errorf("cert CN = %q, want %q", cert.Subject.CommonName, deviceID)
	}
}

// TestIntegration_Join_ReconnectWithMTLS verifies that after Join, the agent
// can reconnect using the returned client certificate and call an authenticated
// RPC (Register).
func TestIntegration_Join_ReconnectWithMTLS(t *testing.T) {
	h := hubtest.StartTestHub(t)
	unauthClient := dialNoClientCert(t, h)

	deviceID := "mtls-dev-" + uuid.New().String()

	joinResp, err := unauthClient.Join(context.Background(), &pb.JoinRequest{
		DeviceId: deviceID,
		Nickname: "mtls-alice-" + uuid.New().String(),
	})
	if err != nil {
		t.Fatalf("Join RPC: %v", err)
	}
	if !joinResp.Success {
		t.Fatalf("Join failed: %s", joinResp.Error)
	}

	// Reconnect with mTLS using the received certificate.
	authedClient := dialWithClientCert(t, h, joinResp.ClientCert, joinResp.ClientKey)

	regResp, err := authedClient.Register(context.Background(), &pb.RegisterRequest{
		SshPort:         22,
		ProtocolVersion: int32(common.ProtocolVersion),
	})
	if err != nil {
		t.Fatalf("Register RPC: %v", err)
	}
	if !regResp.Success {
		t.Fatalf("Register failed: %s", regResp.Error)
	}

	// The registered device must appear in the DevicesOnline list.
	found := false
	for _, d := range regResp.DevicesOnline {
		if d.DeviceId == deviceID {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("registered device %q not found in DevicesOnline", deviceID)
	}
}

// TestIntegration_Join_DuplicateNickname verifies that a second Join with the
// same nickname returns success=false and a non-empty error message.
func TestIntegration_Join_DuplicateNickname(t *testing.T) {
	h := hubtest.StartTestHub(t)
	client := dialNoClientCert(t, h)

	ctx := context.Background()
	nickname := "dup-nick-" + uuid.New().String()

	resp1, err := client.Join(ctx, &pb.JoinRequest{DeviceId: "dup-d1-" + uuid.New().String(), Nickname: nickname})
	if err != nil {
		t.Fatalf("first Join RPC: %v", err)
	}
	if !resp1.Success {
		t.Fatalf("first Join failed: %s", resp1.Error)
	}

	resp2, err := client.Join(ctx, &pb.JoinRequest{DeviceId: "dup-d2-" + uuid.New().String(), Nickname: nickname})
	if err != nil {
		t.Fatalf("second Join RPC transport error: %v", err)
	}
	if resp2.Success {
		t.Error("expected second Join to fail for duplicate nickname, got success=true")
	}
	if resp2.Error == "" {
		t.Error("expected non-empty error message for duplicate nickname")
	}
}
