package clierrors

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"
)

func TestFormat_AlreadyExistsWithContext(t *testing.T) {
	err := grpcstatus.Error(codes.AlreadyExists, "nickname already taken")

	got := Format(Wrap(err, &Context{Nickname: "alice"}), nil)
	want := `error: nickname "alice" is already in use; choose a different one`

	require.Equal(t, want, got)
}

func TestFormat_Unauthenticated(t *testing.T) {
	err := grpcstatus.Error(codes.Unauthenticated, "client certificate required")

	got := Format(err, nil)
	want := `error: not joined to this hub; run "hubfuse join <hub-address>" first`

	require.Equal(t, want, got)
}

func TestFormat_DeviceNotFound(t *testing.T) {
	err := grpcstatus.Error(codes.NotFound, `no device with nickname "bob"`)

	got := Format(err, nil)
	want := `error: device "bob" not found`

	require.Equal(t, want, got)
}

func TestIsNicknameTaken(t *testing.T) {
	statusErr := grpcstatus.Error(codes.AlreadyExists, "nickname already taken")
	stringErr := errors.New("rpc error: code = AlreadyExists desc = nickname already taken")

	require.True(t, IsNicknameTaken(statusErr))
	require.True(t, IsNicknameTaken(stringErr))
}

func TestFormat_FallsBackToOriginal(t *testing.T) {
	err := errors.New("plain failure")
	got := Format(err, nil)
	want := "error: plain failure"

	require.Equal(t, want, got)
}
