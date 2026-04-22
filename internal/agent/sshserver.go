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

// Start begins listening for SSH connections on the configured port.
// It runs until ctx is cancelled or Stop is called.
func (s *SSHServer) Start(ctx context.Context) error {
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", s.port))
	if err != nil {
		return fmt.Errorf("listen on port %d: %w", s.port, err)
	}

	s.mu.Lock()
	s.listener = ln
	s.mu.Unlock()

	s.logger.Info("ssh server listening", "port", s.port)

	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			// Check if context was cancelled or listener was closed.
			select {
			case <-ctx.Done():
				return nil
			default:
				s.logger.Error("accept ssh connection", "error", err)
				return err
			}
		}
		go s.handleConn(conn)
	}
}

// Stop closes the listener, causing Start to return. It is safe to call
// Stop after Start's ctx-cancel path has already closed the listener:
// net.ErrClosed is treated as success so double-close during shutdown
// (signal cancels ctx → Daemon.Shutdown calls Stop) does not surface.
func (s *SSHServer) Stop() error {
	s.mu.Lock()
	ln := s.listener
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
