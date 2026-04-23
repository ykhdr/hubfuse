package hub_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/ykhdr/hubfuse/internal/common"
	"github.com/ykhdr/hubfuse/internal/hub/hubtest"
	pb "github.com/ykhdr/hubfuse/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
)

// dialNoClientCert connects to the hub without presenting a client certificate.
// The client still verifies the server certificate using the given CA PEM.
func dialNoClientCert(t *testing.T, addr string, caCertPEM []byte) pb.HubFuseClient {
	t.Helper()

	caPool := x509.NewCertPool()
	require.True(t, caPool.AppendCertsFromPEM(caCertPEM), "dialNoClientCert: failed to parse CA cert PEM")

	tlsCfg := &tls.Config{
		RootCAs:    caPool,
		MinVersion: tls.VersionTLS13,
	}

	conn, err := grpc.Dial(addr, grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg))) //nolint:staticcheck
	require.NoError(t, err, "grpc.Dial (no cert)")
	t.Cleanup(func() { conn.Close() })

	return pb.NewHubFuseClient(conn)
}

// dialWithClientCert connects to the hub presenting the given mTLS client cert.
func dialWithClientCert(t *testing.T, addr string, certPEM, keyPEM, caCertPEM []byte) pb.HubFuseClient {
	t.Helper()

	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	require.NoError(t, err, "dialWithClientCert: X509KeyPair")

	caPool := x509.NewCertPool()
	require.True(t, caPool.AppendCertsFromPEM(caCertPEM), "dialWithClientCert: failed to parse CA cert PEM")

	tlsCfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      caPool,
		MinVersion:   tls.VersionTLS13,
	}

	conn, err := grpc.Dial(addr, grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg))) //nolint:staticcheck
	require.NoError(t, err, "grpc.Dial (with cert)")
	t.Cleanup(func() { conn.Close() })

	return pb.NewHubFuseClient(conn)
}

// TestIntegration_JoinRegisterSubscribeHeartbeatDeregister runs a full
// device lifecycle against an in-process hub:
//  1. Join (no client cert) — receive signed cert
//  2. Reconnect with mTLS
//  3. Register — verify success and devices list
//  4. Open Subscribe stream — verify DeviceOnline event received
//  5. Heartbeat — verify success
//  6. Deregister — verify DeviceOffline event on the stream
func TestIntegration_JoinRegisterSubscribeHeartbeatDeregister(t *testing.T) {
	h := hubtest.StartTestHub(t)

	// ── 1. Join without a client certificate ────────────────────────────────
	unauthClient := dialNoClientCert(t, h.Addr, h.CAPEM)

	deviceID := "dev-" + uuid.New().String()

	joinToken, _, err := h.Registry.IssueJoinToken(context.Background())
	require.NoError(t, err, "IssueJoinToken")

	joinResp, err := unauthClient.Join(context.Background(), &pb.JoinRequest{
		DeviceId:  deviceID,
		Nickname:  "tester",
		JoinToken: joinToken,
	})
	require.NoError(t, err, "Join RPC")
	require.True(t, joinResp.Success, "Join failed: %s", joinResp.Error)
	require.NotEmpty(t, joinResp.ClientCert, "Join: ClientCert is empty")
	require.NotEmpty(t, joinResp.ClientKey, "Join: ClientKey is empty")
	require.NotEmpty(t, joinResp.CaCert, "Join: CaCert is empty")

	// ── 2. Reconnect with mTLS using the received certificate ───────────────
	authedClient := dialWithClientCert(t, h.Addr, joinResp.ClientCert, joinResp.ClientKey, joinResp.CaCert)

	// ── 3. Register ──────────────────────────────────────────────────────────
	regResp, err := authedClient.Register(context.Background(), &pb.RegisterRequest{
		SshPort:         22,
		ProtocolVersion: int32(common.ProtocolVersion),
	})
	require.NoError(t, err, "Register RPC")
	require.True(t, regResp.Success, "Register failed: %s", regResp.Error)

	found := false
	for _, d := range regResp.DevicesOnline {
		if d.DeviceId == deviceID {
			found = true
			break
		}
	}
	assert.True(t, found, "Register: registered device %q not in DevicesOnline list", deviceID)

	// ── 4. Open Subscribe stream + verify DeviceOnline ───────────────────────
	// Subscribe as device1 and register a second device — device1's stream
	// should receive the DeviceOnline event for device2.
	subscribeCtx, cancelSubscribe := context.WithCancel(context.Background())
	t.Cleanup(cancelSubscribe)

	stream, err := authedClient.Subscribe(subscribeCtx, &pb.SubscribeRequest{})
	require.NoError(t, err, "Subscribe RPC")
	// Consume SubscribeReady sentinel before expecting real events.
	ev, err := stream.Recv()
	require.NoError(t, err, "Subscribe ready")
	require.NotNil(t, ev.GetSubscribeReady(), "expected SubscribeReady, got %T", ev.GetPayload())

	// Join + register device2.
	device2ID := "dev-" + uuid.New().String()
	joinToken2, _, err := h.Registry.IssueJoinToken(context.Background())
	require.NoError(t, err, "IssueJoinToken2")
	joinResp2, err := unauthClient.Join(context.Background(), &pb.JoinRequest{
		DeviceId:  device2ID,
		Nickname:  "tester2",
		JoinToken: joinToken2,
	})
	require.NoError(t, err, "Join2")
	require.True(t, joinResp2.Success, "Join2 resp=%+v", joinResp2)

	authedClient2 := dialWithClientCert(t, h.Addr, joinResp2.ClientCert, joinResp2.ClientKey, joinResp2.CaCert)

	regResp2, err := authedClient2.Register(context.Background(), &pb.RegisterRequest{
		SshPort:         22,
		ProtocolVersion: int32(common.ProtocolVersion),
	})
	require.NoError(t, err, "Register2")
	require.True(t, regResp2.Success, "Register2 resp=%+v", regResp2)

	// Receive DeviceOnline event for device2 on device1's stream.
	eventCh := make(chan *pb.Event, 1)
	go func() {
		ev, err := stream.Recv()
		if err != nil {
			return
		}
		eventCh <- ev
	}()

	select {
	case ev := <-eventCh:
		if assert.NotNil(t, ev.GetDeviceOnline(), "expected DeviceOnline event, got %T", ev.GetPayload()) {
			assert.Equal(t, device2ID, ev.GetDeviceOnline().DeviceId)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for DeviceOnline event on Subscribe stream")
	}

	// ── 5. Heartbeat ─────────────────────────────────────────────────────────
	hbResp, err := authedClient.Heartbeat(context.Background(), &pb.HeartbeatRequest{})
	require.NoError(t, err, "Heartbeat RPC")
	assert.True(t, hbResp.Success, "Heartbeat: success = false")

	// ── 6. Deregister device2 → DeviceOffline on device1's stream ────────────
	offlineCh := make(chan *pb.Event, 1)
	go func() {
		ev, err := stream.Recv()
		if err != nil {
			return
		}
		offlineCh <- ev
	}()

	deregResp, err := authedClient2.Deregister(context.Background(), &pb.DeregisterRequest{})
	require.NoError(t, err, "Deregister RPC")
	assert.True(t, deregResp.Success, "Deregister: success = false")

	select {
	case ev := <-offlineCh:
		if assert.NotNil(t, ev.GetDeviceOffline(), "expected DeviceOffline event, got %T", ev.GetPayload()) {
			assert.Equal(t, device2ID, ev.GetDeviceOffline().DeviceId)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for DeviceOffline event on Subscribe stream")
	}
}

// TestIntegration_AuthenticatedRPCBlockedWithoutCert verifies that
// authenticated RPCs are rejected when no client certificate is presented.
func TestIntegration_AuthenticatedRPCBlockedWithoutCert(t *testing.T) {
	h := hubtest.StartTestHub(t)

	unauthClient := dialNoClientCert(t, h.Addr, h.CAPEM)

	_, err := unauthClient.Register(context.Background(), &pb.RegisterRequest{
		SshPort:         22,
		ProtocolVersion: int32(common.ProtocolVersion),
	})
	assert.Error(t, err, "expected error calling Register without client cert")
}

// TestIntegration_JoinNicknameConflict verifies that a duplicate nickname
// results in success=false without a transport error.
func TestIntegration_JoinNicknameConflict(t *testing.T) {
	h := hubtest.StartTestHub(t)
	client := dialNoClientCert(t, h.Addr, h.CAPEM)

	ctx := context.Background()

	tok1, _, err := h.Registry.IssueJoinToken(ctx)
	require.NoError(t, err, "IssueJoinToken 1")
	resp1, err := client.Join(ctx, &pb.JoinRequest{DeviceId: "d1", Nickname: "clash", JoinToken: tok1})
	require.NoError(t, err, "first Join")
	require.True(t, resp1.GetSuccess(), "first Join success=false")

	tok2, _, err := h.Registry.IssueJoinToken(ctx)
	require.NoError(t, err, "IssueJoinToken 2")
	resp2, err := client.Join(ctx, &pb.JoinRequest{DeviceId: "d2", Nickname: "clash", JoinToken: tok2})
	require.NoError(t, err, "second Join RPC transport error")
	assert.False(t, resp2.Success, "expected second Join to fail for duplicate nickname")
	assert.NotEmpty(t, resp2.Error, "expected non-empty error message for duplicate nickname")
}

func TestRequestPairing_DeviceNotFound(t *testing.T) {
	h := hubtest.StartTestHub(t)
	unauthClient := dialNoClientCert(t, h.Addr, h.CAPEM)

	dnfToken, _, err := h.Registry.IssueJoinToken(context.Background())
	require.NoError(t, err, "IssueJoinToken")
	joinResp, err := unauthClient.Join(context.Background(), &pb.JoinRequest{
		DeviceId:  "dev-pair-from",
		Nickname:  "pair-from",
		JoinToken: dnfToken,
	})
	require.NoError(t, err, "Join")
	require.True(t, joinResp.GetSuccess(), "Join success=false")

	client := dialWithClientCert(t, h.Addr, joinResp.ClientCert, joinResp.ClientKey, h.CAPEM)
	_, err = client.Register(context.Background(), &pb.RegisterRequest{
		SshPort:         2222,
		ProtocolVersion: int32(common.ProtocolVersion),
	})
	require.NoError(t, err, "Register")

	// Pair with non-existent device.
	_, err = client.RequestPairing(context.Background(), &pb.RequestPairingRequest{
		ToDevice:  "nonexistent",
		PublicKey: "ssh-ed25519 AAAA...",
	})
	require.Error(t, err, "expected error for non-existent device")
	st := status.Convert(err)
	assert.Equal(t, codes.NotFound, st.Code())
	assert.Contains(t, st.Message(), "no device with nickname")
}

func TestRequestPairing_DeviceOffline(t *testing.T) {
	h := hubtest.StartTestHub(t)
	unauthClient := dialNoClientCert(t, h.Addr, h.CAPEM)

	// Join two devices but only register one.
	doTok1, _, err := h.Registry.IssueJoinToken(context.Background())
	require.NoError(t, err, "IssueJoinToken dev1")
	joinResp1, err := unauthClient.Join(context.Background(), &pb.JoinRequest{
		DeviceId:  "dev-pair-1",
		Nickname:  "pair-alice",
		JoinToken: doTok1,
	})
	require.NoError(t, err, "Join dev1")
	require.True(t, joinResp1.GetSuccess(), "Join dev1 success=false")

	doTok2, _, err := h.Registry.IssueJoinToken(context.Background())
	require.NoError(t, err, "IssueJoinToken dev2")
	joinResp2, err := unauthClient.Join(context.Background(), &pb.JoinRequest{
		DeviceId:  "dev-pair-2",
		Nickname:  "pair-bob",
		JoinToken: doTok2,
	})
	require.NoError(t, err, "Join dev2")
	require.True(t, joinResp2.GetSuccess(), "Join dev2 success=false")

	// Register only device 1.
	client1 := dialWithClientCert(t, h.Addr, joinResp1.ClientCert, joinResp1.ClientKey, h.CAPEM)
	_, err = client1.Register(context.Background(), &pb.RegisterRequest{
		SshPort:         2222,
		ProtocolVersion: int32(common.ProtocolVersion),
	})
	require.NoError(t, err, "Register")

	// Pair with offline device 2.
	_, err = client1.RequestPairing(context.Background(), &pb.RequestPairingRequest{
		ToDevice:  "pair-bob",
		PublicKey: "ssh-ed25519 AAAA...",
	})
	require.Error(t, err, "expected error for offline device")
	st := status.Convert(err)
	assert.Equal(t, codes.Unavailable, st.Code())
	assert.Contains(t, st.Message(), "device offline")
}

func TestListDevices(t *testing.T) {
	h := hubtest.StartTestHub(t)

	// Join two devices.
	unauthClient := dialNoClientCert(t, h.Addr, h.CAPEM)

	ldTok1, _, err := h.Registry.IssueJoinToken(context.Background())
	require.NoError(t, err, "IssueJoinToken dev1")
	joinResp1, err := unauthClient.Join(context.Background(), &pb.JoinRequest{
		DeviceId:  "dev-list-1",
		Nickname:  "list-alice",
		JoinToken: ldTok1,
	})
	require.NoError(t, err, "Join dev1")
	require.True(t, joinResp1.GetSuccess(), "Join dev1 success=false")

	ldTok2, _, err := h.Registry.IssueJoinToken(context.Background())
	require.NoError(t, err, "IssueJoinToken dev2")
	joinResp2, err := unauthClient.Join(context.Background(), &pb.JoinRequest{
		DeviceId:  "dev-list-2",
		Nickname:  "list-bob",
		JoinToken: ldTok2,
	})
	require.NoError(t, err, "Join dev2")
	require.True(t, joinResp2.GetSuccess(), "Join dev2 success=false")

	// Register only device 1 (device 2 stays in registered state, not yet online).
	client1 := dialWithClientCert(t, h.Addr, joinResp1.ClientCert, joinResp1.ClientKey, h.CAPEM)
	_, err = client1.Register(context.Background(), &pb.RegisterRequest{
		SshPort:         2222,
		ProtocolVersion: int32(common.ProtocolVersion),
	})
	require.NoError(t, err, "Register dev1")

	// Call ListDevices as device 1.
	resp, err := client1.ListDevices(context.Background(), &pb.ListDevicesRequest{})
	require.NoError(t, err, "ListDevices")
	require.Len(t, resp.Devices, 2)

	statusMap := map[string]string{}
	for _, d := range resp.Devices {
		statusMap[d.Nickname] = d.Status
	}
	assert.Equal(t, "online", statusMap["list-alice"])
	assert.Equal(t, "registered", statusMap["list-bob"])
}
