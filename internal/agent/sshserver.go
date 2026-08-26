package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/pkg/sftp"
	gossh "golang.org/x/crypto/ssh"
)

// SSHServer is an embedded SSH/SFTP server that serves share aliases to
// authenticated peer devices.
type SSHServer struct {
	config                *gossh.ServerConfig
	deviceIDByFingerprint map[string]string // key.Marshal() -> device_id
	port                  int
	listener              net.Listener
	logger                *slog.Logger
	mu                    sync.RWMutex

	// stopping records that Stop was asked for, so Serve can tell a deliberate
	// close from a listener that died under it. Listen clears it, so a server
	// that is stopped and re-listened starts from a clean verdict. Guarded by
	// mu together with listener — the two are decided together. (#90)
	stopping bool

	// acls is the current ACL snapshot swapped in by UpdateShares. Readers in
	// the SFTP handler dereference this pointer on every request, so
	// fsnotify-driven config reloads take effect on the next operation.
	acls atomic.Pointer[[]ShareACL]

	// resolver maps device_id -> nickname for ACL tokens that reference
	// human-readable nicknames. Optional: when nil the handler falls back
	// to device_id-only matching.
	resolver DeviceResolver
}

// NewSSHServer creates a new SSHServer that listens on port and uses
// hostKeyPath as the host key (agent's SSH private key).
func NewSSHServer(port int, hostKeyPath string, logger *slog.Logger) (*SSHServer, error) {
	hostKeyBytes, err := os.ReadFile(hostKeyPath)
	if err != nil {
		return nil, fmt.Errorf("read host key %q: %w", hostKeyPath, err)
	}

	signer, err := gossh.ParsePrivateKey(hostKeyBytes)
	if err != nil {
		return nil, fmt.Errorf("parse host key %q: %w", hostKeyPath, err)
	}

	s := &SSHServer{
		deviceIDByFingerprint: make(map[string]string),
		port:                  port,
		logger:                logger,
	}
	empty := []ShareACL{}
	s.acls.Store(&empty)

	cfg := &gossh.ServerConfig{
		PublicKeyCallback: s.publicKeyCallback,
	}
	cfg.AddHostKey(signer)
	s.config = cfg

	return s, nil
}

// publicKeyCallback is the SSH public key authentication callback.
// It accepts a connection when the presented key matches a paired device and
// propagates the device_id to downstream handlers via Permissions.Extensions.
func (s *SSHServer) publicKeyCallback(_ gossh.ConnMetadata, key gossh.PublicKey) (*gossh.Permissions, error) {
	s.mu.RLock()
	deviceID, ok := s.deviceIDByFingerprint[string(key.Marshal())]
	s.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("public key not authorized")
	}
	return &gossh.Permissions{
		Extensions: map[string]string{"hubfuse-device-id": deviceID},
	}, nil
}

// UpdateShares replaces the current ACL snapshot. The slice is copied so the
// caller is free to mutate its own buffer after the call returns.
func (s *SSHServer) UpdateShares(shares []ShareACL) {
	cp := append([]ShareACL(nil), shares...)
	s.acls.Store(&cp)
}

// aclSnapshot returns the current ACL snapshot. Used by the SFTP handler and
// by tests.
func (s *SSHServer) aclSnapshot() []ShareACL {
	p := s.acls.Load()
	if p == nil {
		return nil
	}
	return *p
}

// SetDeviceResolver installs a resolver that maps device_id -> nickname. The
// resolver is consulted by the SFTP handler when matching ACL tokens.
func (s *SSHServer) SetDeviceResolver(r DeviceResolver) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resolver = r
}

func (s *SSHServer) currentResolver() DeviceResolver {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.resolver
}

// UpdateAllowedKeys replaces the set of paired peers. The map is keyed by
// device_id; the server rebuilds the fingerprint->device_id reverse index
// used by publicKeyCallback.
func (s *SSHServer) UpdateAllowedKeys(keys map[string]gossh.PublicKey) {
	idx := make(map[string]string, len(keys))
	for id, k := range keys {
		idx[string(k.Marshal())] = id
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deviceIDByFingerprint = idx
}

// Start begins listening for SSH connections on the configured port and serves
// them until ctx is cancelled or Stop is called.
//
// Start is Listen followed by Serve, and exists only for callers that want both
// in one blocking call. The daemon deliberately does NOT use it: it needs the
// bind error back on its own goroutine, before it tells the hub anything (see
// Daemon.startSSH and issue #90).
func (s *SSHServer) Start(ctx context.Context) error {
	if err := s.Listen(); err != nil {
		return err
	}
	return s.Serve(ctx)
}

// Listen binds the configured port and returns the bind error to the CALLER.
//
// Splitting it out of Start is the whole of issue #90's startup half. Start ran
// inside a goroutine whose error nothing could observe, so a port this process
// could not take — a leftover daemon on 2222, another service, a squatter — was
// reported as one Error line and then ignored: the daemon registered anyway and
// the hub handed every peer an SSH port this process does not own. Peers then
// mount from whoever DOES own it. Binding synchronously is what lets startSSH
// fail before Register is ever called.
//
// There is deliberately no retry here. The obvious objection is that a bind
// failure might be transient — a restart racing the socket of the process it
// replaces — but that race does not exist in this codebase, and it was measured
// rather than assumed (Linux; darwin was not measured): `hubfuse restart` only
// starts the replacement after SignalStop has CONFIRMED the old process is gone
// (SIGTERM, waitForExit, SIGKILL escalation), and a re-bind after a confirmed
// exit succeeded on the first attempt in 5/5 runs at 29-64µs — even with a
// connected socket left behind in TIME_WAIT, because Go's net.Listen sets
// SO_REUSEADDR. Every bind failure that remains — another process on the port,
// EACCES below 1024 — holds until a human acts, so retrying would only delay
// the diagnosis that this issue exists to deliver. (#90)
func (s *SSHServer) Listen() error {
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", s.port))
	if err != nil {
		return fmt.Errorf("listen on port %d: %w", s.port, err)
	}

	s.mu.Lock()
	s.listener = ln
	s.stopping = false
	s.mu.Unlock()

	s.logger.Info("ssh server listening", "port", s.port)
	return nil
}

// acceptRetryInitial/acceptRetryMax bound the backoff Serve applies to a
// transient accept failure. The shape (and the 1s ceiling) is net/http's:
// Server.Serve rides out exactly this class of error rather than dying on it.
const (
	acceptRetryInitial = 5 * time.Millisecond
	acceptRetryMax     = 1 * time.Second
)

// Serve runs the accept loop on the listener Listen bound. It returns nil when
// the server was stopped deliberately (ctx cancelled, or Stop called), and the
// accept error otherwise.
//
// That distinction is the runtime half of issue #90, and it is what makes the
// SSH server's liveness something the daemon reports rather than something it
// merely logs. Before this, ANY accept failure ended the loop and startSSH
// logged it, leaving a registered, online daemon with nothing behind the port
// it advertises — the same defect as the bind case, reached through a different
// door.
//
// Transient failures are ridden out instead of being reported as death, and the
// class is narrow on purpose. Checked against the toolchain's own source rather
// than from memory (go1.26.7, internal/poll.(*FD).Accept): EINTR, EAGAIN and
// ECONNABORTED never reach us — the poller retries them itself. What does reach
// us and is genuinely momentary is resource exhaustion: EMFILE/ENFILE when the
// process or the machine is out of descriptors, ENOBUFS/ENOMEM when the kernel
// is out of memory. In all four the listening socket is still OURS and pending
// connections are still queued in its backlog; "we could not take a descriptor
// for a moment" is not "this device has nothing to serve". Today one of them
// kills the SSH server permanently and silently. Anything else — a closed or
// otherwise dead listener — is death, and the daemon acts on it.
//
// net.Error.Temporary() would classify these too, but it is deprecated
// (staticcheck SA1019, which this repo enables), so the errnos are named.
func (s *SSHServer) Serve(ctx context.Context) error {
	s.mu.RLock()
	ln := s.listener
	s.mu.RUnlock()
	if ln == nil {
		return fmt.Errorf("serve ssh: listener not bound; call Listen first")
	}

	// The watcher is bounded by THIS call, not by ctx. Serve can now return
	// without ctx ever being cancelled (the fatal-accept path above), and a
	// watcher parked on <-ctx.Done() would then outlive the loop it was closing
	// for — holding the listener for the daemon's whole life. (#90)
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = ln.Close()
		case <-done:
		}
	}()

	backoff := acceptRetryInitial
	for {
		conn, err := ln.Accept()
		if err != nil {
			// Deliberate shutdown, asked for in either of the two ways it can
			// be asked for. ctx covers the signal path; s.stopping covers a
			// direct Stop() and is checked explicitly rather than inferred from
			// net.ErrClosed, because "the listener is closed" is exactly what a
			// listener that DIED also looks like. (#90)
			if ctx.Err() != nil || s.isStopping() {
				return nil
			}
			if isTransientAcceptError(err) {
				s.logger.Warn("accept ssh connection failed transiently; retrying",
					"error", err,
					"backoff", backoff,
					"port", s.port,
				)
				select {
				case <-ctx.Done():
					return nil
				case <-time.After(backoff):
				}
				if backoff *= 2; backoff > acceptRetryMax {
					backoff = acceptRetryMax
				}
				continue
			}
			s.logger.Error("accept ssh connection", "error", err)
			return err
		}
		backoff = acceptRetryInitial
		go s.handleConn(conn)
	}
}

// isTransientAcceptError reports whether err is the momentary resource
// exhaustion Serve rides out. See Serve for why exactly these four. (#90)
func isTransientAcceptError(err error) bool {
	return errors.Is(err, syscall.EMFILE) ||
		errors.Is(err, syscall.ENFILE) ||
		errors.Is(err, syscall.ENOBUFS) ||
		errors.Is(err, syscall.ENOMEM)
}

// isStopping reports whether Stop has been called since the last Listen.
func (s *SSHServer) isStopping() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.stopping
}

// Stop closes the listener, causing Serve to return. It is safe to call
// Stop after Serve's ctx-cancel path has already closed the listener:
// net.ErrClosed is treated as success so double-close during shutdown
// (signal cancels ctx → Daemon.Shutdown calls Stop) does not surface.
//
// The stopping flag is raised BEFORE the close, so a Serve that wakes on the
// resulting error can already tell this apart from a listener that died on its
// own. Raising it afterwards would leave a window in which a deliberate stop
// looks like a failure and takes the daemon down with it. (#90)
func (s *SSHServer) Stop() error {
	s.mu.Lock()
	ln := s.listener
	s.stopping = true
	s.mu.Unlock()

	if ln == nil {
		return nil
	}
	if err := ln.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		return err
	}
	return nil
}

// handleConn performs the SSH handshake, extracts the caller's device_id from
// Permissions.Extensions, and dispatches channels.
func (s *SSHServer) handleConn(conn net.Conn) {
	defer conn.Close()

	sshConn, chans, reqs, err := gossh.NewServerConn(conn, s.config)
	if err != nil {
		s.logger.Warn("ssh handshake failed", "remote", conn.RemoteAddr(), "error", err)
		return
	}
	defer sshConn.Close()

	deviceID := ""
	if sshConn.Permissions != nil {
		deviceID = sshConn.Permissions.Extensions["hubfuse-device-id"]
	}
	s.logger.Info("ssh connection established",
		"remote", sshConn.RemoteAddr(),
		"user", sshConn.User(),
		"device_id", deviceID,
	)

	go gossh.DiscardRequests(reqs)

	for newChan := range chans {
		go s.handleChannel(newChan, deviceID)
	}
}

// handleChannel handles a single SSH channel. Only the "session" type with
// the "sftp" subsystem is supported.
func (s *SSHServer) handleChannel(newChan gossh.NewChannel, deviceID string) {
	if newChan.ChannelType() != "session" {
		_ = newChan.Reject(gossh.UnknownChannelType, "unsupported channel type")
		return
	}

	channel, requests, err := newChan.Accept()
	if err != nil {
		s.logger.Error("accept channel", "error", err)
		return
	}
	defer channel.Close()

	for req := range requests {
		if req.Type != "subsystem" {
			if req.WantReply {
				_ = req.Reply(false, nil)
			}
			continue
		}

		// Parse the subsystem name (4-byte length prefix + name).
		if len(req.Payload) < 4 {
			if req.WantReply {
				_ = req.Reply(false, nil)
			}
			continue
		}
		nameLen := int(req.Payload[0])<<24 | int(req.Payload[1])<<16 | int(req.Payload[2])<<8 | int(req.Payload[3])
		if 4+nameLen > len(req.Payload) {
			if req.WantReply {
				_ = req.Reply(false, nil)
			}
			continue
		}
		subsystemName := string(req.Payload[4 : 4+nameLen])

		if subsystemName != "sftp" {
			if req.WantReply {
				_ = req.Reply(false, nil)
			}
			continue
		}

		if req.WantReply {
			_ = req.Reply(true, nil)
		}

		s.serveSFTP(channel, deviceID)
		return
	}
}

// serveSFTP starts a request-based SFTP server on the channel. The handler is
// bound to this connection's device_id and the current ACL snapshot; each
// request re-reads the snapshot so hot-reloaded config takes effect live.
func (s *SSHServer) serveSFTP(channel gossh.Channel, deviceID string) {
	h := newACLHandlers(deviceID, s.currentResolver(), s.aclSnapshot, s.logger)
	srv := sftp.NewRequestServer(channel, h.ToHandlers())
	defer srv.Close()

	if err := srv.Serve(); err != nil {
		// EOF is expected when the client disconnects.
		s.logger.Debug("sftp session ended", "device_id", deviceID, "error", err)
	}
}
