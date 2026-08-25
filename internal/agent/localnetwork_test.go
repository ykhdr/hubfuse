package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/ykhdr/hubfuse/proto"
)

// macDenialError is the verbatim error a hubfuse daemon got for every dial once
// macOS had cut it off from the local network, captured on the test bed while
// reproducing issue #74. Kept literal because the classifier's whole job is to
// recognise THIS, after it has been through a gRPC status and lost its wrapped
// errno.
var macDenialError = status.Error(codes.Unavailable,
	`connection error: desc = "transport: Error while dialing: `+
		`dial tcp 192.168.31.158:9090: connect: no route to host"`)

func TestIsLocalNetworkDenial(t *testing.T) {
	tests := []struct {
		name    string
		goos    string
		hubAddr string
		err     error
		want    bool
	}{
		{
			name:    "the measured failure",
			goos:    "darwin",
			hubAddr: "192.168.31.158:9090",
			err:     macDenialError,
			want:    true,
		},
		{
			name:    "same failure with the errno chain still intact",
			goos:    "darwin",
			hubAddr: "10.0.0.5:9090",
			err:     fmt.Errorf("dial: %w", syscall.EHOSTUNREACH),
			want:    true,
		},
		{
			name:    "an mDNS hub name is local by definition",
			goos:    "darwin",
			hubAddr: "khudorozhkov-2.local:9090",
			err:     macDenialError,
			want:    true,
		},
		{
			// The single most important false case. EHOSTUNREACH to a routable
			// address is a routing problem; blaming macOS for it would swap one
			// misleading message for another.
			name:    "same error, public hub address",
			goos:    "darwin",
			hubAddr: "203.0.113.10:9090",
			err:     macDenialError,
			want:    false,
		},
		{
			name:    "not darwin",
			goos:    "linux",
			hubAddr: "192.168.31.158:9090",
			err:     macDenialError,
			want:    false,
		},
		{
			name:    "an ordinary refused connection is not a denial",
			goos:    "darwin",
			hubAddr: "192.168.31.158:9090",
			err:     status.Error(codes.Unavailable, "connection refused"),
			want:    false,
		},
		{
			name:    "the hub answering that a device is offline",
			goos:    "darwin",
			hubAddr: "192.168.31.158:9090",
			err:     status.Error(codes.Unavailable, `device "bob" is offline`),
			want:    false,
		},
		{
			name:    "a cancelled context",
			goos:    "darwin",
			hubAddr: "192.168.31.158:9090",
			err:     context.Canceled,
			want:    false,
		},
		{
			name:    "no error at all",
			goos:    "darwin",
			hubAddr: "192.168.31.158:9090",
			err:     nil,
			want:    false,
		},
		{
			name:    "no hub address known",
			goos:    "darwin",
			hubAddr: "",
			err:     macDenialError,
			want:    false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, isLocalNetworkDenial(tc.goos, tc.hubAddr, tc.err))
		})
	}
}

func TestIsLocalNetworkAddress(t *testing.T) {
	local := []string{
		"192.168.31.158:9090", "10.0.0.1:9090", "172.16.5.4:9090",
		"169.254.10.1:9090", "127.0.0.1:9090", "[::1]:9090",
		"hubhost.local:9090", "HubHost.Local.:9090", "192.168.1.1",
	}
	for _, addr := range local {
		assert.True(t, isLocalNetworkAddress(addr), "expected %q to read as local", addr)
	}

	// 172.32.x is deliberately in the list: it is one octet outside the
	// 172.16.0.0/12 private block and is the classic off-by-one in hand-rolled
	// private-range checks.
	remote := []string{
		"203.0.113.10:9090", "8.8.8.8:9090", "172.32.0.1:9090",
		"hub.example.com:9090", "", "localhost:9090",
	}
	for _, addr := range remote {
		assert.False(t, isLocalNetworkAddress(addr), "expected %q not to read as local", addr)
	}
}

// TestLocalNetworkDenialMessage_SaysWhatCannotBeGuessed pins the three facts
// that took a live reproduction to establish and that no amount of reading the
// error would reveal: the block is per binary, an SSH-started daemon can never
// be approved, and a rebuild voids the approval.
func TestLocalNetworkDenialMessage_SaysWhatCannotBeGuessed(t *testing.T) {
	msg := strings.ToLower(localNetworkDenialMessage())

	assert.Contains(t, msg, "per binary")
	assert.Contains(t, msg, "ssh")
	assert.Contains(t, msg, "install-agent", "the message must name the command that fixes it")
	assert.Contains(t, msg, "local network", "and the settings pane, for anyone who missed the prompt")
	assert.Contains(t, msg, "voids the approval", "an upgrade re-denies the binary; that must be said")
}

// TestReconnectSession_NamesLocalNetworkDenialOnceAfterAStreak drives the real
// reconnect loop and pins three things at once:
//
//  1. one failure does not accuse macOS — that is an ordinary network blip, and
//     a daemon that cried wolf on every wifi handover would be worse than one
//     that said nothing;
//  2. a sustained streak does, at Error;
//  3. it is said once, not once per retry, for as long as the outage lasts.
func TestReconnectSession_NamesLocalNetworkDenialOnceAfterAStreak(t *testing.T) {
	if syscall.EHOSTUNREACH == 0 {
		t.Skip("EHOSTUNREACH not defined on this platform")
	}
	d, _ := buildTestDaemon(t)
	d.goos = "darwin"
	d.config.Hub.Address = "192.168.31.158:9090"
	d.minReconnectInterval = time.Millisecond

	buf := &syncBuffer{}
	d.logger = captureLogger(buf)

	var attempts atomic.Int32
	d.registerFn = func(context.Context, []*pb.Share, int) (*pb.RegisterResponse, error) {
		attempts.Add(1)
		return nil, macDenialError
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for attempts.Load() < int32(localNetworkFailureStreak)+3 {
			time.Sleep(time.Millisecond)
		}
		cancel()
	}()
	d.reconnectSession(ctx)

	logged := buf.String()
	assert.Equal(t, 1, strings.Count(logged, "install-agent"),
		"the denial must be named exactly once no matter how long the outage runs")
	assert.Contains(t, logged, "level=ERROR")
}

// TestReconnectSession_DoesNotAccuseMacOSOfAnOrdinaryFailure is the negative
// control for the streak: a hub that is merely refusing connections must never
// produce the macOS message, however long the loop runs.
func TestReconnectSession_DoesNotAccuseMacOSOfAnOrdinaryFailure(t *testing.T) {
	d, _ := buildTestDaemon(t)
	// darwin here too: the point is that the PLATFORM is right and the FAILURE
	// is ordinary, so nothing but the error itself can be what keeps it quiet.
	d.goos = "darwin"
	d.config.Hub.Address = "192.168.31.158:9090"
	d.minReconnectInterval = time.Millisecond

	buf := &syncBuffer{}
	d.logger = captureLogger(buf)

	var attempts atomic.Int32
	d.registerFn = func(context.Context, []*pb.Share, int) (*pb.RegisterResponse, error) {
		attempts.Add(1)
		return nil, errors.New("connection refused")
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for attempts.Load() < 8 {
			time.Sleep(time.Millisecond)
		}
		cancel()
	}()
	d.reconnectSession(ctx)

	assert.NotContains(t, buf.String(), "install-agent",
		"an ordinary connection failure must not be reported as a macOS permission problem")
}
