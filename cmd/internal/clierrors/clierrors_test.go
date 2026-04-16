package clierrors

import (
	"errors"
	"testing"

	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"
)

func TestFormatAlreadyExistsWithContext(t *testing.T) {
	err := grpcstatus.Error(codes.AlreadyExists, "nickname already taken")

	got := Format(Wrap(err, &Context{Nickname: "alice"}), nil)
	want := `error: nickname "alice" is already in use; choose a different one`

	if got != want {
		t.Fatalf("Format() = %q, want %q", got, want)
	}
}

func TestFormatUnauthenticated(t *testing.T) {
	err := grpcstatus.Error(codes.Unauthenticated, "client certificate required")

	got := Format(err, nil)
	want := `error: not joined to this hub; run "hubfuse join <hub-address>" first`

	if got != want {
		t.Fatalf("Format() = %q, want %q", got, want)
	}
}

func TestFormatDeviceNotFound(t *testing.T) {
	err := grpcstatus.Error(codes.NotFound, `no device with nickname "bob"`)

	got := Format(err, nil)
	want := `error: device "bob" not found`

	if got != want {
		t.Fatalf("Format() = %q, want %q", got, want)
	}
}

func TestIsNicknameTaken(t *testing.T) {
	statusErr := grpcstatus.Error(codes.AlreadyExists, "nickname already taken")
	stringErr := errors.New("rpc error: code = AlreadyExists desc = nickname already taken")

	if !IsNicknameTaken(statusErr) {
		t.Fatal("expected IsNicknameTaken to detect status error")
	}
	if !IsNicknameTaken(stringErr) {
		t.Fatal("expected IsNicknameTaken to detect string error")
	}
}

func TestFormatFallsBackToOriginal(t *testing.T) {
	err := errors.New("plain failure")
	got := Format(err, nil)
	want := "error: plain failure"

	if got != want {
		t.Fatalf("Format() = %q, want %q", got, want)
	}
}
