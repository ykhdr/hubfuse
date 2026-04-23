package common

import (
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
)
