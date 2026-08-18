package agent

import (
	"context"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/ykhdr/hubfuse/internal/common"
	pb "github.com/ykhdr/hubfuse/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
)

// Hub-connection keepalive. Without it a hub connection that dies half-open —
// the agent's socket stays ESTABLISHED while the hub's side is already gone —
// is indistinguishable from an idle one: the Subscribe stream never ends, so
// the supervisor never reconnects, and every unary RPC blocks trying to open a
// stream on a transport nobody will ever answer. The device then stays offline
// forever and its shares stay unmounted on every peer. (#72)
//
// hubKeepaliveTime is deliberately a third of the hub's 30s liveness timeout
// (hub.DefaultHeartbeatTimeout): a dead transport is detected within
// Time+Timeout = 15s, leaving the agent time to re-establish the session before
// the hub gives up on it. PermitWithoutStream is required rather than optional
// — between sessions there is no open stream, and that is exactly when the
// connection still needs checking. The hub's EnforcementPolicy.MinTime must
// stay below hubKeepaliveTime or the hub answers these pings with
// GOAWAY too_many_pings (see hub.ServerOptions).
//
// Against a hub too old to carry that policy the consequence is smaller than it
// sounds, and it was measured (issue #78). The punishment needs three pings
// with no response written in between, and a running daemon never gets there:
// grpc-go skips the ping entirely whenever the transport was read from within
// hubKeepaliveTime, and the 10s heartbeat guarantees a read that often. Five
// minutes against a real pre-#72 hub produced zero pings and zero disconnects.
// So the two intervals are coupled in a way neither file states on its own:
// this value is a ceiling on the heartbeat cadence, not just a liveness knob.
// Raise defaultHeartbeatInterval past three of these and an old hub starts
// killing the connection every ~30s — see heartbeat.go, which warns about
// exactly that, and bounds_test.go, which pins the ordering.
const (
	hubKeepaliveTime    = 10 * time.Second
	hubKeepaliveTimeout = 5 * time.Second
)

// dialOptions returns the options every hub connection is built with. Both
// dials go through it so the pinned bootstrap connection and the long-lived
// mTLS one cannot drift apart in how they detect a dead hub. (#72)
//
// WithDisableServiceConfig is a hardening measure, not a tuning knob. The
// default scheme for grpc.NewClient is dns, and grpc-go's DNS resolver reads a
// grpc_config TXT record for the target by default (GRPC_ENABLE_TXT_SERVICE_CONFIG
// defaults to true). That means anyone able to answer DNS on the local network
// could hand this client a service config — including waitForReady, which turns
// "the hub is unreachable" from an immediate failure into a call that blocks
// until its deadline. The daemon's own decisions (the register budget, and what
// it concludes when that budget expires) must not be configurable by whoever
// runs the LAN's DNS. (#77)
func dialOptions(creds credentials.TransportCredentials) []grpc.DialOption {
	return []grpc.DialOption{
		grpc.WithTransportCredentials(creds),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                hubKeepaliveTime,
			Timeout:             hubKeepaliveTimeout,
			PermitWithoutStream: true,
		}),
		grpc.WithDisableServiceConfig(),
	}
}

// ErrHubRejected marks an application-level refusal: the RPC itself completed
// (nil gRPC error) but the hub answered Success=false. Callers that must tell
// "the hub is unreachable, keep retrying" apart from "the hub answered and said
// no" match on this — Daemon.Shutdown uses it so a deregistration the hub
// refuses (typically because it already pruned the device) does not turn a
// clean shutdown into a failure. (#69)
var ErrHubRejected = errors.New("hub rejected the request")

// isKeepalivePunished reports whether err is the transport dying because a hub
// answered this agent's keepalive PINGs with GOAWAY too_many_pings — the
// failure mode hubKeepaliveTime's comment warns about: a hub older than #72
// runs grpc-go's default EnforcementPolicy (MinTime: 5m) and treats a 10s
// ping cadence as abuse.
//
// The match is a plain substring test on err.Error() against "too_many_pings",
// and deliberately not a typed check. grpc-go formats the GOAWAY frame's debug
// data into the Unavailable status delivered to every stream on the dying
// transport (internal/transport/http2_client.go:1417-1419 builds
// goAwayDebugMessage, :1052-1053 embeds it), so "too_many_pings" survives
// intact inside err.Error() through any %w wrapping this package or its
// caller adds. The typed reason (GetGoAwayReason) lives on grpc-go's
// unexported internal/transport types and is not reachable from here. And the
// text itself is not grpc-go's to begin with — it is the debug string the
// SERVER writes into the GOAWAY frame (google.golang.org/grpc's
// internal/transport/http2_server.go:913 hardcodes "too_many_pings"), so it is
// frozen in every already-deployed old hub regardless of which grpc-go version
// this agent is built against; only the wrapper wording around it could change.
func isKeepalivePunished(err error) bool {
	return err != nil && strings.Contains(err.Error(), "too_many_pings")
}

// HubClient wraps a gRPC connection to the hub.
type HubClient struct {
	conn   *grpc.ClientConn
	client pb.HubFuseClient
	logger *slog.Logger
}

// DialPinned creates a HubClient for the Join bootstrap using TLS with
// certificate pinning. InsecureSkipVerify is set but the VerifyPeerCertificate
// callback enforces the expected fingerprint, so the connection is not
// actually insecure — the pin is the security control.
func DialPinned(hubAddr, expectedFP string, logger *slog.Logger) (*HubClient, error) {
	tlsCfg := &tls.Config{
		MinVersion:         tls.VersionTLS13,
		InsecureSkipVerify: true, //nolint:gosec // pinning via VerifyPeerCertificate callback below
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			if len(rawCerts) == 0 {
				return common.ErrHubFingerprintMismatch
			}
			actual := common.FingerprintFromCertDER(rawCerts[0])
			if subtle.ConstantTimeCompare([]byte(actual), []byte(expectedFP)) != 1 {
				return common.ErrHubFingerprintMismatch
			}
			return nil
		},
	}
	creds := credentials.NewTLS(tlsCfg)

	conn, err := grpc.NewClient(hubAddr, dialOptions(creds)...)
	if err != nil {
		return nil, fmt.Errorf("dial hub %q (pinned): %w", hubAddr, err)
	}

	return &HubClient{
		conn:   conn,
		client: pb.NewHubFuseClient(conn),
		logger: logger,
	}, nil
}

// DialWithMTLS creates a HubClient using mutual TLS with the given certificates.
func DialWithMTLS(hubAddr, caCertPath, clientCertPath, clientKeyPath string, logger *slog.Logger) (*HubClient, error) {
	tlsCfg, err := common.LoadTLSClientConfig(caCertPath, clientCertPath, clientKeyPath)
	if err != nil {
		return nil, fmt.Errorf("load mTLS config: %w", err)
	}

	return dialWithCreds(hubAddr, credentials.NewTLS(tlsCfg), logger)
}

// dialWithCreds builds a HubClient over already-loaded credentials. It exists so
// the daemon can dial the hub again — see Connector.Dial — without re-reading
// three certificate files from disk on every attempt. Beyond the pointless I/O
// on a recovery path, re-reading is actively unsafe: `hubfuse join` writes
// ca.crt, client.crt and client.key with three separate os.WriteFile calls, so a
// dial landing between them would load a mismatched pair, and a repeat join can
// mint a different device_id — which would silently change the CN, i.e. the
// identity the hub sees, underneath a running daemon. (#77)
//
// grpc.NewClient is lazy: this establishes no TCP connection and performs no TLS
// handshake, so it cannot block and has nothing to time out.
func dialWithCreds(hubAddr string, creds credentials.TransportCredentials, logger *slog.Logger) (*HubClient, error) {
	conn, err := grpc.NewClient(hubAddr, dialOptions(creds)...)
	if err != nil {
		return nil, fmt.Errorf("dial hub %q (mTLS): %w", hubAddr, err)
	}

	return &HubClient{
		conn:   conn,
		client: pb.NewHubFuseClient(conn),
		logger: logger,
	}, nil
}

// Join calls the hub's Join RPC to register a new device and obtain certificates.
func (c *HubClient) Join(ctx context.Context, deviceID, nickname, joinToken string) (*pb.JoinResponse, error) {
	resp, err := c.client.Join(ctx, &pb.JoinRequest{
		DeviceId:  deviceID,
		Nickname:  nickname,
		JoinToken: joinToken,
	})
	if err != nil {
		return nil, fmt.Errorf("Join RPC: %w", err)
	}
	return resp, nil
}

// Register announces this device to the hub with its current shares and SSH port.
//
// A hub that refuses the registration answers with a nil gRPC error and
// Success=false (see hub.Server.Register) — the transport call worked, the
// application-level request did not. Returning that as success made a daemon
// whose identity the hub had pruned log "registered with hub" and write its PID
// file while the hub knew nothing about it, so the failure is translated into a
// real Go error here. resp.Error carries the hub's own, actionable text. (#69)
func (c *HubClient) Register(ctx context.Context, shares []*pb.Share, sshPort int) (*pb.RegisterResponse, error) {
	resp, err := c.client.Register(ctx, &pb.RegisterRequest{
		Shares:          shares,
		SshPort:         int32(sshPort),
		ProtocolVersion: int32(common.ProtocolVersion),
	})
	if err != nil {
		return nil, fmt.Errorf("Register RPC: %w", err)
	}
	if !resp.GetSuccess() {
		return nil, fmt.Errorf("%w: registration refused: %s", ErrHubRejected, rejectionReason(resp.GetError()))
	}
	return resp, nil
}

// Rename requests a nickname change for this device. An application-level
// refusal (nickname taken) comes back as Success=false and is returned as an
// error, so callers only have to handle err. (#69)
func (c *HubClient) Rename(ctx context.Context, newNickname string) (*pb.RenameResponse, error) {
	resp, err := c.client.Rename(ctx, &pb.RenameRequest{
		NewNickname: newNickname,
	})
	if err != nil {
		return nil, fmt.Errorf("Rename RPC: %w", err)
	}
	if !resp.GetSuccess() {
		return nil, fmt.Errorf("%w: rename refused: %s", ErrHubRejected, rejectionReason(resp.GetError()))
	}
	return resp, nil
}

// Heartbeat sends a liveness ping to the hub. A hub that does not know this
// device (pruned identity, or a hub restarted on an empty database) answers
// Success=false with a nil gRPC error; that must surface as an error, otherwise
// the daemon keeps "heartbeating" into the void while the hub counts it as
// absent. HeartbeatResponse carries no error string, so the message is
// generic — the actionable one comes from the Register path. (#69)
func (c *HubClient) Heartbeat(ctx context.Context) error {
	resp, err := c.client.Heartbeat(ctx, &pb.HeartbeatRequest{})
	if err != nil {
		return fmt.Errorf("Heartbeat RPC: %w", err)
	}
	if !resp.GetSuccess() {
		// HeartbeatResponse carries no reason, and the hub refuses for more than
		// one cause (unknown device, store failure), so name the likeliest one
		// without asserting it. The hub logs which it was.
		return fmt.Errorf("%w: heartbeat refused — the hub may no longer know this device (re-join may be required)", ErrHubRejected)
	}
	return nil
}

// UpdateShares pushes the current share list to the hub. (#69: Success=false is
// an error, not a silent no-op — a share list the hub never stored would leave
// peers mounting shares that are not published.)
func (c *HubClient) UpdateShares(ctx context.Context, shares []*pb.Share) error {
	resp, err := c.client.UpdateShares(ctx, &pb.UpdateSharesRequest{
		Shares: shares,
	})
	if err != nil {
		return fmt.Errorf("UpdateShares RPC: %w", err)
	}
	if !resp.GetSuccess() {
		return fmt.Errorf("%w: share update refused", ErrHubRejected)
	}
	return nil
}

// Deregister removes this device from the hub. (#69: Success=false is an error
// so shutdown reports a deregistration the hub did not perform.)
func (c *HubClient) Deregister(ctx context.Context) error {
	resp, err := c.client.Deregister(ctx, &pb.DeregisterRequest{})
	if err != nil {
		return fmt.Errorf("Deregister RPC: %w", err)
	}
	if !resp.GetSuccess() {
		return fmt.Errorf("%w: deregistration refused", ErrHubRejected)
	}
	return nil
}

// rejectionReason renders the error string of an application-level refusal,
// substituting a placeholder when the hub sent none — an empty reason would
// otherwise produce a dangling "hub rejected registration: " line.
func rejectionReason(reason string) string {
	if reason == "" {
		return "no reason given"
	}
	return reason
}

// Leave deregisters this device permanently. The hub deletes the device row
// and all dependent shares/pairings. Returns nil on success.
func (c *HubClient) Leave(ctx context.Context) error {
	resp, err := c.client.Leave(ctx, &pb.LeaveRequest{})
	if err != nil {
		return fmt.Errorf("Leave RPC: %w", err)
	}
	if !resp.Success {
		return fmt.Errorf("leave failed: %s", resp.Error)
	}
	return nil
}

// Subscribe opens a server-streaming RPC to receive events from the hub.
// The hub identifies the subscriber from the mTLS client certificate.
func (c *HubClient) Subscribe(ctx context.Context) (pb.HubFuse_SubscribeClient, error) {
	stream, err := c.client.Subscribe(ctx, &pb.SubscribeRequest{})
	if err != nil {
		return nil, fmt.Errorf("Subscribe RPC: %w", err)
	}
	return stream, nil
}

// RequestPairing initiates pairing with another device and returns the invite code.
func (c *HubClient) RequestPairing(ctx context.Context, toDevice, publicKey string) (string, error) {
	resp, err := c.client.RequestPairing(ctx, &pb.RequestPairingRequest{
		ToDevice:  toDevice,
		PublicKey: publicKey,
	})
	if err != nil {
		return "", fmt.Errorf("RequestPairing RPC: %w", err)
	}
	return resp.InviteCode, nil
}

// ListDevices retrieves all devices known to the hub.
func (c *HubClient) ListDevices(ctx context.Context) (*pb.ListDevicesResponse, error) {
	resp, err := c.client.ListDevices(ctx, &pb.ListDevicesRequest{})
	if err != nil {
		return nil, fmt.Errorf("ListDevices RPC: %w", err)
	}
	return resp, nil
}

// ConfirmPairing completes a pairing handshake and returns the peer's public
// key, device ID, and nickname. The hub identifies the caller from the mTLS
// client certificate. Application-level failures (invalid/expired invite,
// attempts exceeded) come back as Success=false with a nil gRPC error — those
// are translated into a real Go error so the CLI doesn't print a misleading
// "paired with ..." line on failure.
func (c *HubClient) ConfirmPairing(ctx context.Context, inviteCode, publicKey string) (peerPublicKey, peerDeviceID, peerNickname string, err error) {
	resp, err := c.client.ConfirmPairing(ctx, &pb.ConfirmPairingRequest{
		InviteCode: inviteCode,
		PublicKey:  publicKey,
	})
	if err != nil {
		return "", "", "", fmt.Errorf("ConfirmPairing RPC: %w", err)
	}
	if !resp.Success {
		return "", "", "", fmt.Errorf("confirm pairing: %s", resp.Error)
	}
	return resp.PeerPublicKey, resp.PeerDeviceId, resp.PeerNickname, nil
}

// Close shuts down the underlying gRPC connection.
func (c *HubClient) Close() error {
	if err := c.conn.Close(); err != nil {
		return fmt.Errorf("close hub connection: %w", err)
	}
	return nil
}
