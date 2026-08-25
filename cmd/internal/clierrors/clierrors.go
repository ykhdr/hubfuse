package clierrors

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/ykhdr/hubfuse/internal/common"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"
)

// Context carries optional metadata that helps render a more specific message.
type Context struct {
	Nickname string
	HubAddr  string
}

// Error wraps an error with optional context for downstream formatting.
type Error struct {
	Err error
	Ctx *Context
}

func (e Error) Error() string {
	return e.Err.Error()
}

func (e Error) Unwrap() error {
	return e.Err
}

// Wrap attaches context to an error for later formatting.
func Wrap(err error, ctx *Context) error {
	if err == nil {
		return nil
	}
	return Error{Err: err, Ctx: ctx}
}

// Format renders a human-friendly error string suitable for CLI output.
// It understands gRPC status errors (including strings that look like them)
// and falls back to the original error text.
func Format(err error, defaultCtx *Context) string {
	if err == nil {
		return ""
	}

	ctx := Context{}
	if defaultCtx != nil {
		ctx = *defaultCtx
	}

	var withCtx Error
	if errors.As(err, &withCtx) {
		err = withCtx.Err
		if withCtx.Ctx != nil {
			ctx = *withCtx.Ctx
		}
	}

	// Translate well-known sentinel errors to friendly messages.
	if errors.Is(err, common.ErrJoinTokenMissingFingerprint) {
		return "error: join token must include hub fingerprint — regenerate with 'hubfuse-hub issue-join'"
	}
	if errors.Is(err, common.ErrHubFingerprintMismatch) {
		return "error: hub TLS fingerprint does not match the token — possible MITM attack; regenerate the token"
	}

	if msg, ok := translateStatus(err, ctx); ok {
		return "error: " + msg
	}

	return "error: " + err.Error()
}

// IsNicknameTaken reports whether the error corresponds to an AlreadyExists
// status for nickname conflicts.
func IsNicknameTaken(err error) bool {
	st, ok := statusFromError(err)
	if !ok {
		return false
	}
	return st.Code() == codes.AlreadyExists && strings.Contains(strings.ToLower(st.Message()), "nickname")
}

var statusRe = regexp.MustCompile(`code = ([A-Za-z_]+) desc = (.+)`)
var quotedDoubleRe = regexp.MustCompile(`"([^"]+)"`)
var quotedSingleRe = regexp.MustCompile(`'([^']+)'`)

func statusFromError(err error) (*grpcstatus.Status, bool) {
	st, ok := grpcstatus.FromError(err)
	if ok {
		return st, true
	}

	msg := err.Error()

	if mapped, ok := statusFromMessage(msg); ok {
		return mapped, true
	}

	m := statusRe.FindStringSubmatch(msg)
	if len(m) != 3 {
		return nil, false
	}

	code, ok := codeFromString(m[1])
	if !ok {
		return nil, false
	}
	return grpcstatus.New(code, strings.TrimSpace(m[2])), true
}

func codeFromString(name string) (codes.Code, bool) {
	normalized := strings.ToUpper(strings.ReplaceAll(name, "_", ""))
	switch normalized {
	case "CANCELED", "CANCELLED":
		return codes.Canceled, true
	case "UNKNOWN":
		return codes.Unknown, true
	case "INVALIDARGUMENT":
		return codes.InvalidArgument, true
	case "DEADLINEEXCEEDED":
		return codes.DeadlineExceeded, true
	case "NOTFOUND":
		return codes.NotFound, true
	case "ALREADYEXISTS":
		return codes.AlreadyExists, true
	case "PERMISSIONDENIED":
		return codes.PermissionDenied, true
	case "RESOURCEEXHAUSTED":
		return codes.ResourceExhausted, true
	case "FAILEDPRECONDITION":
		return codes.FailedPrecondition, true
	case "ABORTED":
		return codes.Aborted, true
	case "OUTOFRANGE":
		return codes.OutOfRange, true
	case "UNIMPLEMENTED":
		return codes.Unimplemented, true
	case "INTERNAL":
		return codes.Internal, true
	case "UNAVAILABLE":
		return codes.Unavailable, true
	case "DATALOSS":
		return codes.DataLoss, true
	case "UNAUTHENTICATED":
		return codes.Unauthenticated, true
	default:
		return codes.Unknown, false
	}
}

func statusFromMessage(msg string) (*grpcstatus.Status, bool) {
	lower := strings.ToLower(msg)
	switch {
	case strings.Contains(lower, "nickname already taken"):
		return grpcstatus.New(codes.AlreadyExists, msg), true
	case strings.Contains(lower, "device not found") || strings.Contains(lower, "no device with nickname"):
		return grpcstatus.New(codes.NotFound, msg), true
	case strings.Contains(lower, "device offline") || strings.Contains(lower, "not currently connected"):
		return grpcstatus.New(codes.Unavailable, msg), true
	case strings.Contains(lower, "unsupported protocol version"):
		return grpcstatus.New(codes.FailedPrecondition, msg), true
	case strings.Contains(lower, "invalid invite code"):
		return grpcstatus.New(codes.PermissionDenied, msg), true
	case strings.Contains(lower, "max pairing attempts exceeded"):
		return grpcstatus.New(codes.ResourceExhausted, msg), true
	case strings.Contains(lower, "invite code expired"):
		return grpcstatus.New(codes.DeadlineExceeded, msg), true
	case strings.Contains(lower, "client certificate required"):
		return grpcstatus.New(codes.Unauthenticated, msg), true
	case strings.Contains(lower, "devices already paired"):
		return grpcstatus.New(codes.AlreadyExists, msg), true
	default:
		return nil, false
	}
}

func translateStatus(err error, ctx Context) (string, bool) {
	st, ok := statusFromError(err)
	if !ok {
		return "", false
	}

	msg := st.Message()
	switch st.Code() {
	case codes.AlreadyExists:
		if ctx.Nickname != "" {
			return fmt.Sprintf("nickname %q is already in use; choose a different one", ctx.Nickname), true
		}
		if nick := extractNickname(msg); nick != "" {
			return fmt.Sprintf("nickname %q is already in use; choose a different one", nick), true
		}
		return "nickname is already in use; choose a different one", true
	case codes.Unauthenticated:
		return "not joined to this hub; run \"hubfuse join <hub-address>\" first", true
	case codes.NotFound:
		if nick := extractNickname(msg); nick != "" {
			return fmt.Sprintf("device %q not found", nick), true
		}
		if strings.Contains(msg, "device not found") {
			return "device not found", true
		}
		return msg, true
	case codes.Unavailable:
		// Transport failures are checked FIRST, because codes.Unavailable
		// carries two unrelated meanings here and only the content tells them
		// apart: the hub's own service returns it for "that peer device is
		// offline" (see statusFromMessage), and grpc-go returns it for "this
		// client could not reach the hub at all".
		//
		// Asking extractNickname first got the second case badly wrong. grpc-go
		// wraps a dial failure as
		//
		//	connection error: desc = "transport: Error while dialing: dial tcp
		//	192.168.31.158:9090: connect: no route to host"
		//
		// and quotedDoubleRe happily takes the first quoted run it finds, so an
		// operator whose hub was unreachable was told
		//
		//	error: device "transport: Error while dialing: … no route to host" is offline
		//
		// — a transport error presented as the name of a device that does not
		// exist. The HubAddr branch below, which knows how to say this properly,
		// could never be reached, because extractNickname always matched first.
		// Observed on a macOS agent whose local-network access had been revoked
		// (issue #74), where it sent the reader looking for a peer instead of at
		// the hub connection.
		if isTransportFailure(msg) {
			if ctx.HubAddr != "" {
				return fmt.Sprintf("cannot reach hub at %s: %s", ctx.HubAddr, msg), true
			}
			return "cannot reach the hub: " + msg, true
		}
		if nick := extractNickname(msg); nick != "" {
			return fmt.Sprintf("device %q is offline", nick), true
		}
		if strings.Contains(msg, "device offline") {
			return "device is offline", true
		}
		if ctx.HubAddr != "" {
			if msg == "" {
				return fmt.Sprintf("cannot reach hub at %s", ctx.HubAddr), true
			}
			return fmt.Sprintf("cannot reach hub at %s: %s", ctx.HubAddr, msg), true
		}
		if msg != "" {
			return "hub is unavailable: " + msg, true
		}
		return "hub is unavailable", true
	case codes.FailedPrecondition:
		if strings.Contains(msg, "unsupported protocol version") {
			return "this client is incompatible with the hub (protocol mismatch)", true
		}
		return msg, true
	case codes.PermissionDenied:
		if strings.Contains(msg, "invalid invite code") {
			return "invite code is invalid", true
		}
		if ctx.Nickname != "" {
			return fmt.Sprintf("pairing rejected by %q", ctx.Nickname), true
		}
		if nick := extractNickname(msg); nick != "" {
			return fmt.Sprintf("pairing rejected by %q", nick), true
		}
		if strings.Contains(strings.ToLower(msg), "pairing rejected") {
			return "pairing request was rejected", true
		}
		return msg, true
	case codes.ResourceExhausted:
		if strings.Contains(msg, "max pairing attempts exceeded") {
			return "pairing failed: too many attempts; request a new code", true
		}
		return msg, true
	case codes.DeadlineExceeded:
		if strings.Contains(msg, "invite code expired") {
			return "invite code has expired; request a new one", true
		}
		if ctx.HubAddr != "" {
			return fmt.Sprintf("hub at %s did not respond in time", ctx.HubAddr), true
		}
		if msg != "" {
			return "hub did not respond in time: " + msg, true
		}
		return "hub did not respond in time", true
	default:
		if msg == "" {
			return strings.ToLower(st.Code().String()), true
		}
		return msg, true
	}
}

// transportFailureMarkers are phrases only grpc-go's own transport layer
// produces. None of them can occur in the hub's application-level messages,
// which are written in internal/hub/server.go and talk about devices,
// nicknames and shares — so a message carrying one of these is about reaching
// the hub, never about a peer.
//
// Matching on text rather than on a typed error is forced: by the time an error
// reaches this package it is a grpc status whose Message() is a formatted
// string, and the underlying net.OpError (with its syscall.Errno) has not
// survived. The list is deliberately short and specific for that reason —
// every entry is a fixed grpc-go phrasing, not a guess at what a failure might
// look like. (#74)
var transportFailureMarkers = []string{
	"connection error:",         // dial and handshake failures
	"transport:",                // the transport layer naming itself
	"keepalive ping failed",     // a connection that stopped answering (#72)
	"error reading from server", // the peer went away mid-stream
	"last connection error:",    // reported by the pick path after retries
	"name resolver error",       // the address could not be resolved at all
}

// isTransportFailure reports whether msg describes a failure to reach the hub
// rather than an application-level answer from it. See the codes.Unavailable
// branch in translateStatus for why this has to be decided before any attempt
// to read a nickname out of the message. (#74)
func isTransportFailure(msg string) bool {
	lower := strings.ToLower(msg)
	for _, marker := range transportFailureMarkers {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func extractNickname(msg string) string {
	if m := quotedDoubleRe.FindStringSubmatch(msg); len(m) == 2 {
		return m[1]
	}
	if m := quotedSingleRe.FindStringSubmatch(msg); len(m) == 2 {
		return m[1]
	}

	if nick := nextToken(msg, "nickname"); isLikelyNickname(nick) {
		return nick
	}
	if nick := nextToken(msg, "device"); isLikelyNickname(nick) {
		return nick
	}

	return ""
}

func nextToken(msg, keyword string) string {
	fields := strings.Fields(msg)
	for i, f := range fields {
		if strings.EqualFold(strings.Trim(f, "\"'"), keyword) && i+1 < len(fields) {
			return strings.Trim(fields[i+1], "\"'")
		}
	}
	return ""
}

func isLikelyNickname(word string) bool {
	switch strings.ToLower(word) {
	case "", "already", "taken", "is", "not", "device", "nickname", "offline":
		return false
	default:
		return true
	}
}
