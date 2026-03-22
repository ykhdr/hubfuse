package agent

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"

	"github.com/pkg/sftp"
	gossh "golang.org/x/crypto/ssh"
)

// SSHServer is an embedded SSH/SFTP server that serves share aliases to
// authenticated peer devices.
type SSHServer struct {
	config      *gossh.ServerConfig
	shares      map[string]string        // alias -> local path
	allowedKeys map[string]gossh.PublicKey // device_id -> parsed public key
	port        int
	listener    net.Listener
	logger      *slog.Logger
	mu          sync.RWMutex

	// sftpRoot is the directory that contains symlinks alias -> real path.
	sftpRoot string
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

	// Derive sftpRoot from the key file location.
	sftpRoot := filepath.Join(filepath.Dir(hostKeyPath), "sftp-root")

	s := &SSHServer{
		shares:      make(map[string]string),
		allowedKeys: make(map[string]gossh.PublicKey),
		port:        port,
		logger:      logger,
		sftpRoot:    sftpRoot,
	}

	cfg := &gossh.ServerConfig{
		PublicKeyCallback: s.publicKeyCallback,
	}
	cfg.AddHostKey(signer)
	s.config = cfg

	return s, nil
}

// publicKeyCallback is the SSH public key authentication callback.
// It accepts a connection if the provided key matches any of the allowed keys.
func (s *SSHServer) publicKeyCallback(_ gossh.ConnMetadata, key gossh.PublicKey) (*gossh.Permissions, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	keyBytes := key.Marshal()
	for _, allowed := range s.allowedKeys {
		if bytes.Equal(allowed.Marshal(), keyBytes) {
			return &gossh.Permissions{}, nil
		}
	}
	return nil, fmt.Errorf("public key not authorized")
}

// UpdateShares replaces the current alias→path mapping and recreates symlinks
// under sftpRoot so the SFTP handler can resolve them.
func (s *SSHServer) UpdateShares(shares map[string]string) {
	s.mu.Lock()
	s.shares = shares
	s.mu.Unlock()

	if err := s.rebuildSFTPRoot(shares); err != nil {
		s.logger.Error("rebuild sftp root", "error", err)
	}
}

// UpdateAllowedKeys replaces the set of keys that are permitted to connect.
func (s *SSHServer) UpdateAllowedKeys(keys map[string]gossh.PublicKey) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.allowedKeys = keys
}

// rebuildSFTPRoot recreates sftpRoot with one symlink per alias pointing to
// the corresponding local path.
func (s *SSHServer) rebuildSFTPRoot(shares map[string]string) error {
	if err := os.MkdirAll(s.sftpRoot, 0700); err != nil {
		return fmt.Errorf("create sftp root %q: %w", s.sftpRoot, err)
	}

	// Remove existing symlinks.
	entries, err := os.ReadDir(s.sftpRoot)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read sftp root %q: %w", s.sftpRoot, err)
	}
	for _, e := range entries {
		linkPath := filepath.Join(s.sftpRoot, e.Name())
		if err := os.Remove(linkPath); err != nil {
			s.logger.Warn("remove sftp root entry", "path", linkPath, "error", err)
		}
	}

	// Create new symlinks.
	for alias, realPath := range shares {
		linkPath := filepath.Join(s.sftpRoot, alias)
		if err := os.Symlink(realPath, linkPath); err != nil {
			s.logger.Warn("create sftp symlink", "alias", alias, "target", realPath, "error", err)
		}
	}

	return nil
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

// Stop closes the listener, causing Start to return.
func (s *SSHServer) Stop() error {
	s.mu.Lock()
	ln := s.listener
	s.mu.Unlock()

	if ln == nil {
		return nil
	}
	return ln.Close()
}

// handleConn performs the SSH handshake and dispatches channels.
func (s *SSHServer) handleConn(conn net.Conn) {
	defer conn.Close()

	sshConn, chans, reqs, err := gossh.NewServerConn(conn, s.config)
	if err != nil {
		s.logger.Warn("ssh handshake failed", "remote", conn.RemoteAddr(), "error", err)
		return
	}
	defer sshConn.Close()

	s.logger.Info("ssh connection established", "remote", sshConn.RemoteAddr(), "user", sshConn.User())

	// Discard out-of-band requests.
	go gossh.DiscardRequests(reqs)

	// Handle each new channel.
	for newChan := range chans {
		go s.handleChannel(newChan)
	}
}

// handleChannel handles a single SSH channel. Only the "session" type with
// the "sftp" subsystem is supported.
func (s *SSHServer) handleChannel(newChan gossh.NewChannel) {
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

	// Wait for a subsystem request.
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

		s.serveSFTP(channel)
		return
	}
}

// serveSFTP starts an SFTP server on the channel, rooted at sftpRoot so that
// alias symlinks resolve to the correct local paths.
func (s *SSHServer) serveSFTP(channel gossh.Channel) {
	s.mu.RLock()
	root := s.sftpRoot
	s.mu.RUnlock()

	// Ensure root exists before serving.
	if err := os.MkdirAll(root, 0700); err != nil {
		s.logger.Error("ensure sftp root exists", "path", root, "error", err)
		return
	}

	srv, err := sftp.NewServer(
		channel,
		sftp.WithServerWorkingDirectory(root),
	)
	if err != nil {
		s.logger.Error("create sftp server", "error", err)
		return
	}
	defer srv.Close()

	if err := srv.Serve(); err != nil {
		// EOF is expected when the client disconnects.
		s.logger.Debug("sftp session ended", "error", err)
	}
}
