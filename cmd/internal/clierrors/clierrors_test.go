package clierrors

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"
)

func TestFormat_AlreadyExistsWithContext(t *testing.T) {
	err := grpcstatus.Error(codes.AlreadyExists, "nickname already taken")

	got := Format(Wrap(err, &Context{Nickname: "alice"}), nil)
	want := `error: nickname "alice" is already in use; choose a different one`

	assert.Equal(t, want, got)
}

func TestFormat_AlreadyExistsPlainString(t *testing.T) {
	err := errors.New("nickname already taken")

	got := Format(Wrap(err, &Context{Nickname: "bob"}), nil)
	want := `error: nickname "bob" is already in use; choose a different one`

	assert.Equal(t, want, got)
}

func TestFormat_Unauthenticated(t *testing.T) {
	err := grpcstatus.Error(codes.Unauthenticated, "client certificate required")

	got := Format(err, nil)
	want := `error: not joined to this hub; run "hubfuse join <hub-address>" first`

	assert.Equal(t, want, got)
}

func TestFormat_DeviceNotFound(t *testing.T) {
	err := grpcstatus.Error(codes.NotFound, `no device with nickname "bob"`)

	got := Format(err, nil)
	want := `error: device "bob" not found`

	assert.Equal(t, want, got)
}

func TestFormat_InternalWithoutMessage(t *testing.T) {
	err := grpcstatus.Error(codes.Internal, "")

	got := Format(err, nil)
	want := "error: internal"

	assert.Equal(t, want, got)
}

func TestIsNicknameTaken(t *testing.T) {
	statusErr := grpcstatus.Error(codes.AlreadyExists, "nickname already taken")
	stringErr := errors.New("rpc error: code = AlreadyExists desc = nickname already taken")
	plainErr := errors.New("nickname already taken")

	assert.True(t, IsNicknameTaken(statusErr), "IsNicknameTaken(statusErr)")
	assert.True(t, IsNicknameTaken(stringErr), "IsNicknameTaken(stringErr)")
	assert.True(t, IsNicknameTaken(plainErr), "IsNicknameTaken(plainErr)")
}

func TestFormat_FallsBackToOriginal(t *testing.T) {
	err := errors.New("plain failure")
	got := Format(err, nil)
	want := "error: plain failure"

	assert.Equal(t, want, got)
}

func TestFormat_Unavailable_WithHubAddress(t *testing.T) {
	err := grpcstatus.Error(codes.Unavailable, "connection refused")

	got := Format(Wrap(err, &Context{HubAddr: "localhost:9090"}), nil)
	want := "error: cannot reach hub at localhost:9090: connection refused"

	assert.Equal(t, want, got)
}

func TestFormat_DeadlineExceeded_Default(t *testing.T) {
	err := grpcstatus.Error(codes.DeadlineExceeded, "context deadline exceeded")

	got := Format(Wrap(err, &Context{HubAddr: "10.0.0.1:9090"}), nil)
	want := "error: hub at 10.0.0.1:9090 did not respond in time"

	assert.Equal(t, want, got)
}

func TestFormat_PermissionDenied_PairRejected(t *testing.T) {
	err := grpcstatus.Error(codes.PermissionDenied, "pairing rejected by remote device")

	got := Format(Wrap(err, &Context{Nickname: "carol"}), nil)
	want := `error: pairing rejected by "carol"`

	assert.Equal(t, want, got)
}

func TestFormat_FailedPrecondition_UnsupportedProtocol(t *testing.T) {
	err := grpcstatus.Error(codes.FailedPrecondition, "unsupported protocol version")

	got := Format(err, nil)
	want := "error: this client is incompatible with the hub (protocol mismatch)"

	assert.Equal(t, want, got)
}

func TestFormat_UnknownCodeWithMessage_DropsCodePrefix(t *testing.T) {
	err := grpcstatus.Error(codes.Code(99), "too many foos")

	got := Format(err, nil)
	want := "error: too many foos"

	assert.Equal(t, want, got)
}

// ─── An unreachable hub is not an offline device (issue #74) ─────────────────

// unreachableHubMessage is the verbatim grpc status message a hubfuse daemon
// logged on macOS once its local-network access had been revoked — the failure
// issue #74 is about. It is kept as a literal rather than assembled from parts
// because the whole point of the fix is how THIS string is read: gRPC wraps the
// dial error in double quotes, and that quoting is what used to be mistaken for
// a device nickname.
const unreachableHubMessage = `connection error: desc = "transport: Error while dialing: ` +
	`dial tcp 192.168.31.158:9090: connect: no route to host"`

// TestFormat_UnreachableHubIsNotReportedAsAnOfflineDevice pins the fix for the
// second half of issue #74 ("the failure is unreadable"). Before it, this exact
// error printed as
//
//	error: device "transport: Error while dialing: … no route to host" is offline
//
// naming a device that does not exist and hiding the fact that the HUB could
// not be reached. The operator was sent looking for a peer.
func TestFormat_UnreachableHubIsNotReportedAsAnOfflineDevice(t *testing.T) {
	err := grpcstatus.Error(codes.Unavailable, unreachableHubMessage)

	got := Format(Wrap(err, &Context{HubAddr: "192.168.31.158:9090"}), nil)

	assert.Contains(t, got, "cannot reach hub at 192.168.31.158:9090",
		"a transport-level failure must be reported as the hub being unreachable")
	assert.NotContains(t, got, "is offline",
		"it must not be presented as a device that is offline")
	assert.Contains(t, got, "no route to host",
		"the underlying cause must survive into the message — it is what tells the operator "+
			"this is a reachability problem and not, say, a certificate one")
}

// TestFormat_UnreachableHubWithoutContext covers the same failure reported by a
// caller that has no hub address to hand (Wrap with no Context). The message
// must still not invent a device: losing the address is a smaller loss than
// naming something that was never involved.
func TestFormat_UnreachableHubWithoutContext(t *testing.T) {
	err := grpcstatus.Error(codes.Unavailable, unreachableHubMessage)

	got := Format(err, nil)

	assert.Contains(t, got, "cannot reach the hub")
	assert.NotContains(t, got, "is offline")
}

// TestFormat_KeepaliveFailureIsAHubReachabilityProblem covers the other message
// seen in the same macOS session, produced by the keepalive added in #72 when
// the connection is severed underneath a live session. It reaches this package
// with no quoted run at all, so the old code fell through to the generic
// branches by luck rather than by decision; this pins it as a decision.
func TestFormat_KeepaliveFailureIsAHubReachabilityProblem(t *testing.T) {
	err := grpcstatus.Error(codes.Unavailable, "keepalive ping failed to receive ACK within timeout")

	got := Format(Wrap(err, &Context{HubAddr: "192.168.31.158:9090"}), nil)

	assert.Contains(t, got, "cannot reach hub at 192.168.31.158:9090")
	assert.NotContains(t, got, "is offline")
}

// TestFormat_OfflineDeviceStillReadsAsAnOfflineDevice is the negative control,
// and it is the one that matters: codes.Unavailable genuinely carries BOTH
// meanings, and a fix that made every Unavailable a hub problem would be just
// as wrong as the bug it replaced — it would hide "that peer is offline", the
// single most common thing a user of `hubfuse mount` needs to be told.
func TestFormat_OfflineDeviceStillReadsAsAnOfflineDevice(t *testing.T) {
	err := grpcstatus.Error(codes.Unavailable, `device "bob" is offline`)

	got := Format(Wrap(err, &Context{HubAddr: "192.168.31.158:9090"}), nil)
	want := `error: device "bob" is offline`

	assert.Equal(t, want, got,
		"an application-level answer from a reachable hub must be unaffected by the transport check")
}

// TestIsTransportFailure_Table pins the classifier itself, including what it
// must NOT claim. The false cases are hub answers: if any of them started
// matching, the negative control above would break, but so would every other
// device-level message this package translates.
func TestIsTransportFailure_Table(t *testing.T) {
	tests := []struct {
		name string
		msg  string
		want bool
	}{
		{name: "dial failure as grpc wraps it", msg: unreachableHubMessage, want: true},
		{name: "keepalive ping unanswered", msg: "keepalive ping failed to receive ACK within timeout", want: true},
		{name: "stream died mid-read", msg: `error reading from server: EOF`, want: true},
		{name: "resolver could not resolve the address", msg: "name resolver error: produced zero addresses", want: true},

		{name: "hub says a device is offline", msg: `device "bob" is offline`, want: false},
		{name: "hub says a device is not found", msg: `no device with nickname "bob"`, want: false},
		{name: "hub says the nickname is taken", msg: "nickname already taken", want: false},
		{name: "empty", msg: "", want: false},
		// "connection refused" is deliberately NOT a transport marker here: it
		// arrives as a bare message with no grpc transport framing, and the
		// HubAddr branch below already renders it correctly. Adding it would
		// change an existing, tested message for no gain
		// (TestFormat_Unavailable_WithHubAddress).
		{name: "bare connection refused stays out of the transport set", msg: "connection refused", want: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, isTransportFailure(tc.msg))
		})
	}
}
