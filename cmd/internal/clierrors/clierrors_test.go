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
