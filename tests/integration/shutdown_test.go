package integration

import (
	"context"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/ykhdr/hubfuse/internal/common"
	"github.com/ykhdr/hubfuse/internal/hub"
	"github.com/ykhdr/hubfuse/internal/hub/store"
	pb "github.com/ykhdr/hubfuse/proto"
	"github.com/ykhdr/hubfuse/tests/testrelay"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
)

// These tests drive a real hub.NewHub/Start/Stop instead of the hubtest
// harness: hubtest.Harness.Stop already runs the fixed shutdown sequence
// internally (see hubtest.go), which would make it exercise the fix rather
// than let these tests prove it. (#75)

// realHub is a real hub.Hub wired up the way Hub.Start wires one, plus the
// bits a test needs that the production Hub type does not expose: its own
// listen address (Hub never publishes it — the caller picks the port) and
// its CA (Hub never returns it — NewHub only writes it to disk). (#75)
type realHub struct {
	h       *hub.Hub
	addr    string
	dataDir string
	caCert  *x509.Certificate
	caKey   *rsa.PrivateKey
	caPEM   []byte
	// store is a second handle onto the same SQLite file Hub itself opened.
	// WAL mode permits this concurrent access — it is exactly how
	// `hubfuse-hub issue-join` reaches a running hub's database from
	// outside the process (cmd/hubfuse-hub/issue_join.go's openStore) — and
	// it is what lets a test seed a device row without a join token or a
	// Registry reference, neither of which hub.Hub exposes.
	store store.Store
}

// startRealHub builds and starts a real hub.Hub on a free localhost port and
// returns once its listener is bound. Stop is registered via t.Cleanup so a
// test that calls h.Stop() itself still ends up idempotent (Hub.Stop caches
// its outcome — see stopOnce in hub.go).
func startRealHub(t *testing.T) *realHub {
	t.Helper()

	// Hub does not publish its listener address, so the port has to be
	// chosen before Start binds it. (#75)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err, "reserve a free port")
	addr := lis.Addr().String()
	require.NoError(t, lis.Close(), "release the reserved port")

	dataDir := t.TempDir()

	ready := make(chan struct{})
	cfg := hub.Config{
		ListenAddr: addr,
		DataDir:    dataDir,
		OnReady:    func() { close(ready) },
	}

	h, err := hub.NewHub(cfg)
	require.NoError(t, err, "hub.NewHub")

	started := make(chan error, 1)
	go func() { started <- h.Start(context.Background()) }()

	select {
	case <-ready:
	case err := <-started:
		t.Fatalf("hub.Start returned before becoming ready: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("hub did not become ready within 5s")
	}

	t.Cleanup(func() {
		_, _ = h.Stop()
		select {
		case <-started:
		case <-time.After(hub.DefaultShutdownBudget + 2*time.Second):
			t.Error("hub.Start did not return after Stop settled its own budget")
		}
	})

	// hub.NewHub does not return its CA, so read it back off disk exactly
	// the way hub.go's own loadOrGenerateCerts does, and mint client certs
	// directly with it rather than going through Join's single-use-token
	// machinery. (#75)
	tlsDir := filepath.Join(dataDir, common.TLSDir)

	caCertDER, err := common.LoadPEM(filepath.Join(tlsDir, common.CACertFile))
	require.NoError(t, err, "load CA cert")
	caCert, err := x509.ParseCertificate(caCertDER)
	require.NoError(t, err, "parse CA cert")

	caKeyDER, err := common.LoadPEM(filepath.Join(tlsDir, common.CAKeyFile))
	require.NoError(t, err, "load CA key")
	caKey, err := x509.ParsePKCS1PrivateKey(caKeyDER)
	require.NoError(t, err, "parse CA key")

	caPEM, err := os.ReadFile(filepath.Join(tlsDir, common.CACertFile))
	require.NoError(t, err, "read CA PEM")

	dbPath := filepath.Join(dataDir, common.DBFile)
	s, err := store.NewSQLiteStore(dbPath)
	require.NoError(t, err, "open second store handle on the hub's own DB")
	t.Cleanup(func() { _ = s.Close() })

	return &realHub{
		h:       h,
		addr:    addr,
		dataDir: dataDir,
		caCert:  caCert,
		caKey:   caKey,
		caPEM:   caPEM,
		store:   s,
	}
}

// seedDevice writes a device row directly (bypassing Join's token
// consumption, which a test outside the hub process cannot satisfy — see
// realHub.store) and mints a client cert signed by the hub's own CA. The
// returned cert authenticates as deviceID exactly the way a Join-issued one
// would, since AuthUnaryInterceptor only ever reads the CN.
func (rh *realHub) seedDevice(t *testing.T, deviceID, nickname string) (certPEM, keyPEM []byte) {
	t.Helper()

	err := rh.store.CreateDevice(context.Background(), &store.Device{
		DeviceID:      deviceID,
		Nickname:      nickname,
		Status:        store.StatusRegistered,
		LastHeartbeat: time.Now().UTC(),
	})
	require.NoError(t, err, "seed device row")

	certPEM, keyPEM, err = common.SignClientCert(rh.caCert, rh.caKey, deviceID)
	require.NoError(t, err, "sign client cert")
	return certPEM, keyPEM
}

// dial connects to addr trusting rh's CA. A nil certPEM dials without a
// client certificate (for RPCs that don't need one — none of these tests
// exercise Join, so this is currently unused outside of documenting the
// option, but kept symmetric with dialNoClientCert/dialWithClientCert in
// integration_test.go).
func (rh *realHub) dial(t *testing.T, addr string, certPEM, keyPEM []byte) pb.HubFuseClient {
	t.Helper()

	pool := x509.NewCertPool()
	require.True(t, pool.AppendCertsFromPEM(rh.caPEM), "parse CA PEM")

	tlsCfg := &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS13}
	if certPEM != nil {
		cert, err := tls.X509KeyPair(certPEM, keyPEM)
		require.NoError(t, err, "X509KeyPair")
		tlsCfg.Certificates = []tls.Certificate{cert}
	}

	conn, err := grpc.Dial(addr, grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg))) //nolint:staticcheck
	require.NoError(t, err, "grpc.Dial")
	t.Cleanup(func() { _ = conn.Close() })

	return pb.NewHubFuseClient(conn)
}

// register calls Register on client the way a real daemon's first session
// does — no shares, an arbitrary SSH port, the current protocol version.
func register(t *testing.T, client pb.HubFuseClient) {
	t.Helper()
	resp, err := client.Register(context.Background(), &pb.RegisterRequest{
		SshPort:         22,
		ProtocolVersion: int32(common.ProtocolVersion),
	})
	require.NoError(t, err, "Register RPC")
	require.True(t, resp.GetSuccess(), "Register refused: %s", resp.GetError())
}

// TestIntegration_Shutdown_HealthySubscriberStopsFast is the regression test
// for #75's headline bug: before the fix, Hub.Stop called
// grpcServer.GracefulStop(), which waits for every in-flight RPC — and
// Subscribe is a long-lived stream that only ends when its client goes away.
// Measured on real binaries, a single HEALTHY connected agent held the hub
// open for 29.99s (then SIGKILLed by the test harness), almost exactly the
// agent's own 3 x 10s maxHeartbeatFailures x heartbeat-interval budget: the
// hub was waiting for the AGENT to give up on IT.
//
// The fix (CloseAllSubscribers, run right before StopServer) ends every
// subscriber stream itself, so Stop no longer depends on anything the peer
// does. This test asserts that end-to-end: Stop returns quickly, the
// subscriber sees its own DeviceOffline first (the sweep's broadcast is
// queued before the channel closes — see hub.go's sweepOnlineDevices
// comment), and the stream itself then ends cleanly rather than hanging or
// erroring.
func TestIntegration_Shutdown_HealthySubscriberStopsFast(t *testing.T) {
	rh := startRealHub(t)

	deviceID := "shutdown-fast-" + uuid.New().String()
	certPEM, keyPEM := rh.seedDevice(t, deviceID, "shutdown-fast")
	client := rh.dial(t, rh.addr, certPEM, keyPEM)
	register(t, client)

	subCtx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	stream, err := client.Subscribe(subCtx, &pb.SubscribeRequest{})
	require.NoError(t, err, "Subscribe")

	ready, err := stream.Recv()
	require.NoError(t, err, "SubscribeReady")
	require.NotNil(t, ready.GetSubscribeReady(), "expected SubscribeReady, got %T", ready.GetPayload())

	start := time.Now()
	settled, stopErr := rh.h.Stop()
	elapsed := time.Since(start)

	require.NoError(t, stopErr)
	assert.True(t, settled, "a single healthy subscriber must not prevent a settled shutdown")
	// The measured healthy-subscriber case was milliseconds; 2s leaves ample
	// room for CI jitter while still being nowhere near the 29.99s this
	// guards against.
	assert.Less(t, elapsed, 2*time.Second,
		"Stop took too long with a single healthy subscriber — this is the exact #75 regression (measured 29.99s before the fix)")

	// sweepOnlineDevices queues this device's own DeviceOffline before
	// CloseAllSubscribers closes the channel, so it must arrive before the
	// stream ends.
	ev, err := stream.Recv()
	require.NoError(t, err, "expected the shutdown sweep's DeviceOffline before the stream ends")
	require.NotNil(t, ev.GetDeviceOffline(), "expected DeviceOffline, got %T", ev.GetPayload())
	assert.Equal(t, deviceID, ev.GetDeviceOffline().DeviceId)

	// Once the channel closes, server.go's Subscribe handler observes !ok
	// and returns nil — a clean stream end, which the client surfaces as
	// io.EOF, not an error status.
	_, err = stream.Recv()
	assert.ErrorIs(t, err, io.EOF, "the stream must end cleanly (io.EOF) once CloseAllSubscribers closes its channel")
}

// TestIntegration_Shutdown_RefusesNewRPCsAfterDrain exercises the drain guard
// (DrainUnaryInterceptor / DrainStreamInterceptor + Registry.Draining) that
// keeps the shutdown sequence from racing its own clients: without it, a
// reconnect provoked by CloseAllSubscribers could re-enter Register/Subscribe
// on a hub that has already swept its devices offline and already closed its
// subscriptions, undoing the sweep and giving GracefulStop a brand new stream
// to wait for all over again. (#75)
//
// Hub does not expose its Registry, so there is no direct hook to call
// Drain() and observe the guard synchronously. Instead this hammers a
// pre-established connection with RPCs while Stop runs concurrently: Drain()
// is the very first thing stop() does, well before StopServer ever touches
// the gRPC transport, so a tight retry loop reliably lands at least one
// attempt inside that window.
func TestIntegration_Shutdown_RefusesNewRPCsAfterDrain(t *testing.T) {
	rh := startRealHub(t)

	deviceID := "shutdown-drain-" + uuid.New().String()
	certPEM, keyPEM := rh.seedDevice(t, deviceID, "shutdown-drain")
	client := rh.dial(t, rh.addr, certPEM, keyPEM)
	register(t, client)

	// Sanity: this RPC succeeds before shutdown begins.
	hbResp, err := client.Heartbeat(context.Background(), &pb.HeartbeatRequest{})
	require.NoError(t, err, "Heartbeat before shutdown")
	require.True(t, hbResp.GetSuccess())

	stopHammer := make(chan struct{})
	defer close(stopHammer) // let any still-spinning goroutine exit even if a select below times out

	unaryStatus := make(chan *status.Status, 1)
	go func() {
		for {
			select {
			case <-stopHammer:
				return
			default:
			}
			callCtx, cancel := context.WithTimeout(context.Background(), time.Second)
			_, callErr := client.Heartbeat(callCtx, &pb.HeartbeatRequest{})
			cancel()
			if callErr != nil {
				if st, ok := status.FromError(callErr); ok && st.Code() == codes.Unavailable {
					unaryStatus <- st
					return
				}
			}
		}
	}()

	streamStatus := make(chan *status.Status, 1)
	go func() {
		for {
			select {
			case <-stopHammer:
				return
			default:
			}
			callCtx, cancel := context.WithTimeout(context.Background(), time.Second)
			subStream, subErr := client.Subscribe(callCtx, &pb.SubscribeRequest{})
			if subErr == nil {
				_, subErr = subStream.Recv()
			}
			cancel()
			if subErr != nil {
				if st, ok := status.FromError(subErr); ok && st.Code() == codes.Unavailable {
					streamStatus <- st
					return
				}
			}
		}
	}()

	stopDone := make(chan struct{})
	go func() {
		_, stopErr := rh.h.Stop()
		assert.NoError(t, stopErr)
		close(stopDone)
	}()

	// The code alone (codes.Unavailable) does not distinguish the drain guard
	// from ordinary post-shutdown transport teardown, which would also
	// eventually refuse RPCs with the same code even without
	// DrainUnaryInterceptor/DrainStreamInterceptor. The message does:
	// common.ErrHubShuttingDown is the only Unavailable status either
	// interceptor produces, and — verified empirically across repeated runs,
	// since Drain() lands well before the sweep's file-backed SQLite write —
	// it is what a connection hammered this way consistently observes. (#75)
	select {
	case st := <-unaryStatus:
		assert.Equal(t, codes.Unavailable, st.Code(),
			"a unary RPC observed during/after Drain must be refused as Unavailable (#75)")
		assert.Contains(t, st.Message(), "hub is shutting down",
			"the refusal should be DrainUnaryInterceptor's ErrHubShuttingDown, not merely a torn-down transport")
	case <-time.After(hub.DefaultShutdownBudget + 2*time.Second):
		t.Fatal("never observed a unary RPC refused during/after Drain")
	}

	select {
	case st := <-streamStatus:
		assert.Equal(t, codes.Unavailable, st.Code(),
			"a new Subscribe observed during/after Drain must be refused as Unavailable (#75)")
		assert.Contains(t, st.Message(), "hub is shutting down",
			"the refusal should be DrainStreamInterceptor's ErrHubShuttingDown, not merely a torn-down transport")
	case <-time.After(hub.DefaultShutdownBudget + 2*time.Second):
		t.Fatal("never observed a Subscribe refused during/after Drain")
	}

	<-stopDone
}

// TestIntegration_Shutdown_WedgedSubscriberStaysWithinBudget puts
// tests/testrelay between the client and the hub — the half-open connection
// from issue #72, reproduced here to check that a connection nobody can tell
// is dead still does not stop the hub from exiting.
//
// One might expect CloseAllSubscribers alone to settle this quickly: it
// closes the channel from inside the hub process, server.go's Subscribe
// handler observes !ok and returns immediately, and its write of the
// stream's closing HTTP/2 frames does not block (the relay's read loop keeps
// draining the socket even while broken — see testrelay.pipe). That part is
// true, but it is not the whole picture: grpc-go's own GracefulStop
// additionally waits for the transport itself to finish closing, which needs
// a read that the relay's broken connection can never deliver (no FIN, no
// RST — see testrelay.Relay). Measured here, consistently across repeated
// runs: GracefulStop never returns on its own,
// StopServer's grace (3s, shutdownGrace in lifecycle.go) expires, Stop()
// forces the server down, and the whole sequence settles at ~3.0-3.6s —
// comfortably inside DefaultShutdownBudget (6s). This is StopOutcome's
// StopForced path, which stopResultFor documents as the expected, on-schedule
// outcome for exactly this scenario. (#75)
func TestIntegration_Shutdown_WedgedSubscriberStaysWithinBudget(t *testing.T) {
	rh := startRealHub(t)
	relay := testrelay.Start(t, rh.addr)

	deviceID := "shutdown-wedged-" + uuid.New().String()
	certPEM, keyPEM := rh.seedDevice(t, deviceID, "shutdown-wedged")
	client := rh.dial(t, relay.Addr, certPEM, keyPEM)
	register(t, client)

	subCtx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	stream, err := client.Subscribe(subCtx, &pb.SubscribeRequest{})
	require.NoError(t, err, "Subscribe")

	ready, err := stream.Recv()
	require.NoError(t, err, "SubscribeReady")
	require.NotNil(t, ready.GetSubscribeReady(), "expected SubscribeReady, got %T", ready.GetPayload())

	// Silently swallow traffic on this connection without closing the
	// socket: no FIN, no RST, nothing for either side to observe on its own.
	relay.Break()

	start := time.Now()
	settled, stopErr := rh.h.Stop()
	elapsed := time.Since(start)

	require.NoError(t, stopErr)
	assert.LessOrEqual(t, elapsed, hub.DefaultShutdownBudget+2*time.Second,
		"Stop must return within its own budget (plus CI jitter margin) regardless of how the wedged peer's RPC settles")
	// Verified empirically (see the doc comment above), not assumed: forcing
	// the server down after the grace window is itself the on-schedule
	// outcome for a wedged peer, so settled is true here too.
	assert.True(t, settled, "the forced-stop path for a wedged peer still settles inside the budget")
	t.Logf("wedged-subscriber shutdown: settled=%v elapsed=%s", settled, elapsed)
}
