package clierrors

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

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
		code := strings.ToLower(st.Code().String())
		if msg == "" {
			return code, true
		}
		return fmt.Sprintf("%s: %s", code, msg), true
	}
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
