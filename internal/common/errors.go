package common

import (
	"errors"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var (
	ErrNicknameTaken        = status.Error(codes.AlreadyExists, "nickname already taken")
	ErrDeviceNotFound       = status.Error(codes.NotFound, "device not found")
	ErrDeviceOffline        = status.Error(codes.Unavailable, "device offline")
	ErrUnsupportedProtocol  = status.Error(codes.FailedPrecondition, "unsupported protocol version")
	ErrInvalidInviteCode    = status.Error(codes.PermissionDenied, "invalid invite code")
	ErrMaxAttemptsExceeded  = status.Error(codes.ResourceExhausted, "max pairing attempts exceeded")
	ErrInviteExpired        = status.Error(codes.DeadlineExceeded, "invite code expired")
	ErrNotAuthenticated     = status.Error(codes.Unauthenticated, "client certificate required")
	ErrPairingAlreadyExists = status.Error(codes.AlreadyExists, "devices already paired")
	ErrInvalidJoinToken     = status.Error(codes.PermissionDenied, "invalid join token")
	ErrJoinTokenExpired     = status.Error(codes.DeadlineExceeded, "join token expired")

	// ErrJoinTokenMissingFingerprint is returned when a join token lacks the
	// hub fingerprint suffix (the .<fp> part appended by issue-join).
	ErrJoinTokenMissingFingerprint = errors.New("join token must include hub fingerprint (regenerate with 'hubfuse-hub issue-join')")

	// ErrHubFingerprintMismatch is returned when the hub's TLS leaf cert does
	// not match the fingerprint embedded in the join token — possible MITM.
	ErrHubFingerprintMismatch = errors.New("hub TLS fingerprint does not match the token — possible MITM")
)
