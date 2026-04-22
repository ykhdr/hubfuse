package agent

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pkg/sftp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	gossh "golang.org/x/crypto/ssh"
)

// generateTestKeyPair generates an SSH key pair in dir and returns the
// private key signer and the parsed public key.
func generateTestKeyPair(t *testing.T, dir string) (gossh.Signer, gossh.PublicKey) {
	t.Helper()
	_, err := GenerateSSHKeyPair(dir)
	require.NoError(t, err, "GenerateSSHKeyPair()")

	privBytes, err := os.ReadFile(filepath.Join(dir, "id_ed25519"))
	require.NoError(t, err, "read private key")

	signer, err := gossh.ParsePrivateKey(privBytes)
	require.NoError(t, err, "parse private key")

	pubBytes, err := os.ReadFile(filepath.Join(dir, "id_ed25519.pub"))
	require.NoError(t, err, "read public key")

	pub, _, _, _, err := gossh.ParseAuthorizedKey(pubBytes)
	require.NoError(t, err, "parse public key")

	return signer, pub
}

// startTestServer starts an SSHServer on a random port and returns the
// server, its address, and a cancel function.
func startTestServer(t *testing.T, hostKeyPath string) (*SSHServer, string, context.CancelFunc) {
	t.Helper()

	srv, err := NewSSHServer(0, hostKeyPath, discardLogger())
	require.NoError(t, err, "NewSSHServer()")

	// Use port 0 to get a random port.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err, "net.Listen()")
	addr := ln.Addr().String()

	// Override the listener with our pre-bound one.
	srv.mu.Lock()
	srv.listener = ln
	srv.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())

	// Run the server accept loop directly (bypassing the Listen call).
	go func() {
		go func() {
			<-ctx.Done()
			_ = ln.Close()
		}()
		for {
			conn, err := ln.Accept()
			if err != nil {
				select {
				case <-ctx.Done():
					return
				default:
					return
				}
			}
			go srv.handleConn(conn)
		}
	}()

	return srv, addr, cancel
}

// dialSSH connects to addr using signer for public key auth.
func dialSSH(t *testing.T, addr string, signer gossh.Signer) (*gossh.Client, error) {
	t.Helper()
	cfg := &gossh.ClientConfig{
		User:            "hubfuse",
		Auth:            []gossh.AuthMethod{gossh.PublicKeys(signer)},
		HostKeyCallback: gossh.InsecureIgnoreHostKey(),
		Timeout:         5 * time.Second,
	}
	return gossh.Dial("tcp", addr, cfg)
}

// ─── NewSSHServer ─────────────────────────────────────────────────────────────

func TestNewSSHServer_MissingHostKey(t *testing.T) {
	_, err := NewSSHServer(2222, "/does/not/exist/id_ed25519", discardLogger())
	assert.Error(t, err, "NewSSHServer() expected error for missing host key")
}

func TestNewSSHServer_InvalidHostKey(t *testing.T) {
	dir := t.TempDir()
	badKey := filepath.Join(dir, "bad_key")
	require.NoError(t, os.WriteFile(badKey, []byte("not a valid pem key"), 0600), "WriteFile()")

	_, err := NewSSHServer(2222, badKey, discardLogger())
	assert.Error(t, err, "NewSSHServer() expected error for invalid host key")
}

func TestNewSSHServer_ValidHostKey(t *testing.T) {
	dir := t.TempDir()
	_, err := GenerateSSHKeyPair(dir)
	require.NoError(t, err, "GenerateSSHKeyPair()")

	hostKeyPath := filepath.Join(dir, "id_ed25519")
	_, err = NewSSHServer(2222, hostKeyPath, discardLogger())
	require.NoError(t, err, "NewSSHServer()")
}

// ─── Start / Stop ─────────────────────────────────────────────────────────────

func TestSSHServer_StartStop(t *testing.T) {
	dir := t.TempDir()
	_, err := GenerateSSHKeyPair(dir)
	require.NoError(t, err, "GenerateSSHKeyPair()")

	hostKeyPath := filepath.Join(dir, "id_ed25519")
	srv, err := NewSSHServer(0, hostKeyPath, discardLogger())
	require.NoError(t, err, "NewSSHServer()")

	ctx, cancel := context.WithCancel(context.Background())

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err, "net.Listen()")
	addr := ln.Addr().String()

	srv.mu.Lock()
	srv.listener = ln
	srv.mu.Unlock()

	done := make(chan error, 1)
	go func() {
		go func() {
			<-ctx.Done()
			_ = ln.Close()
		}()
		for {
			conn, err := ln.Accept()
			if err != nil {
				select {
				case <-ctx.Done():
					done <- nil
					return
				default:
					done <- err
					return
				}
			}
			go srv.handleConn(conn)
		}
	}()

	// Verify the server is reachable by connecting.
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	require.NoError(t, err, "dial server")
	conn.Close()

	cancel()

	select {
	case err := <-done:
		assert.NoError(t, err, "server exited with error")
	case <-time.After(3 * time.Second):
		t.Fatal("server did not stop within timeout")
	}
}

// ─── Public key authentication ────────────────────────────────────────────────

func TestSSHServer_AllowedKeyAuthentication(t *testing.T) {
	hostDir := t.TempDir()
	clientDir := t.TempDir()

	_, err := GenerateSSHKeyPair(hostDir)
	require.NoError(t, err, "GenerateSSHKeyPair(host)")

	clientSigner, clientPub := generateTestKeyPair(t, clientDir)

	srv, addr, cancel := startTestServer(t, filepath.Join(hostDir, "id_ed25519"))
	defer cancel()

	// Allow the client key.
	srv.UpdateAllowedKeys(map[string]gossh.PublicKey{
		"client-device": clientPub,
	})

	client, err := dialSSH(t, addr, clientSigner)
	require.NoError(t, err, "SSH dial with allowed key")
	defer client.Close()
}

func TestSSHServer_DeniedKeyAuthentication(t *testing.T) {
	hostDir := t.TempDir()
	allowedDir := t.TempDir()
	deniedDir := t.TempDir()

	_, err := GenerateSSHKeyPair(hostDir)
	require.NoError(t, err, "GenerateSSHKeyPair(host)")

	_, allowedPub := generateTestKeyPair(t, allowedDir)
	deniedSigner, _ := generateTestKeyPair(t, deniedDir)

	srv, addr, cancel := startTestServer(t, filepath.Join(hostDir, "id_ed25519"))
	defer cancel()

	// Only allow the allowedPub key, not the deniedSigner key.
	srv.UpdateAllowedKeys(map[string]gossh.PublicKey{
		"allowed-device": allowedPub,
	})

	_, err = dialSSH(t, addr, deniedSigner)
	assert.Error(t, err, "SSH dial with denied key succeeded, expected failure")
}

func TestSSHServer_NoAllowedKeys(t *testing.T) {
	hostDir := t.TempDir()
	clientDir := t.TempDir()

	_, err := GenerateSSHKeyPair(hostDir)
	require.NoError(t, err, "GenerateSSHKeyPair(host)")

	clientSigner, _ := generateTestKeyPair(t, clientDir)

	srv, addr, cancel := startTestServer(t, filepath.Join(hostDir, "id_ed25519"))
	defer cancel()

	// No keys allowed.
	srv.UpdateAllowedKeys(map[string]gossh.PublicKey{})

	_, err = dialSSH(t, addr, clientSigner)
	assert.Error(t, err, "SSH dial with no allowed keys succeeded, expected failure")
}

// ─── UpdateShares / SFTP alias mapping ───────────────────────────────────────

func TestSSHServer_UpdateShares_RebuildsSFTPRoot(t *testing.T) {
	hostDir := t.TempDir()
	_, err := GenerateSSHKeyPair(hostDir)
	require.NoError(t, err, "GenerateSSHKeyPair(host)")

	srv, err := NewSSHServer(0, filepath.Join(hostDir, "id_ed25519"), discardLogger())
	require.NoError(t, err, "NewSSHServer()")

	// Create a real directory to be shared.
	sharedDir := t.TempDir()
	testFile := filepath.Join(sharedDir, "hello.txt")
	require.NoError(t, os.WriteFile(testFile, []byte("hello"), 0644), "WriteFile()")

	shares := map[string]string{"myalias": sharedDir}
	srv.UpdateShares(shares)

	// Verify symlink was created.
	linkPath := filepath.Join(srv.sftpRoot, "myalias")
	info, err := os.Lstat(linkPath)
	require.NoError(t, err, "Lstat(%q)", linkPath)
	assert.NotZero(t, info.Mode()&os.ModeSymlink, "%q is not a symlink", linkPath)

	// Resolve and verify target.
	target, err := os.Readlink(linkPath)
	require.NoError(t, err, "Readlink(%q)", linkPath)
	assert.Equal(t, sharedDir, target, "symlink target")
}

func TestSSHServer_UpdateShares_ReplacesOldSymlinks(t *testing.T) {
	hostDir := t.TempDir()
	_, err := GenerateSSHKeyPair(hostDir)
	require.NoError(t, err, "GenerateSSHKeyPair(host)")

	srv, err := NewSSHServer(0, filepath.Join(hostDir, "id_ed25519"), discardLogger())
	require.NoError(t, err, "NewSSHServer()")

	dir1 := t.TempDir()
	dir2 := t.TempDir()

	srv.UpdateShares(map[string]string{"alias-old": dir1})
	srv.UpdateShares(map[string]string{"alias-new": dir2})

	// Old symlink should be gone.
	_, err = os.Lstat(filepath.Join(srv.sftpRoot, "alias-old"))
	assert.True(t, os.IsNotExist(err), "old symlink still exists after UpdateShares")

	// New symlink should exist.
	_, err = os.Lstat(filepath.Join(srv.sftpRoot, "alias-new"))
	assert.NoError(t, err, "new symlink not found")
}

// ─── SFTP file access ─────────────────────────────────────────────────────────

func TestSSHServer_SFTPFileAccess(t *testing.T) {
	hostDir := t.TempDir()
	clientDir := t.TempDir()
	sharedDir := t.TempDir()

	_, err := GenerateSSHKeyPair(hostDir)
	require.NoError(t, err, "GenerateSSHKeyPair(host)")
	clientSigner, clientPub := generateTestKeyPair(t, clientDir)

	// Write a test file into the shared directory.
	want := "sftp-test-content"
	testFile := filepath.Join(sharedDir, "test.txt")
	require.NoError(t, os.WriteFile(testFile, []byte(want), 0644), "WriteFile()")

	srv, addr, cancel := startTestServer(t, filepath.Join(hostDir, "id_ed25519"))
	defer cancel()

	srv.UpdateAllowedKeys(map[string]gossh.PublicKey{"client": clientPub})
	srv.UpdateShares(map[string]string{"myshare": sharedDir})

	// Dial SSH.
	sshClient, err := dialSSH(t, addr, clientSigner)
	require.NoError(t, err, "SSH dial")
	defer sshClient.Close()

	// Open SFTP session.
	sftpClient, err := sftp.NewClient(sshClient)
	require.NoError(t, err, "sftp.NewClient()")
	defer sftpClient.Close()

	// Read a file through the alias symlink path.
	aliasFilePath := filepath.Join(srv.sftpRoot, "myshare", "test.txt")
	f, err := sftpClient.Open(aliasFilePath)
	require.NoError(t, err, "sftp.Open(%q)", aliasFilePath)
	defer f.Close()

	data, err := io.ReadAll(f)
	require.NoError(t, err, "io.ReadAll()")

	assert.Equal(t, want, string(data), "file contents")
}

// ─── UpdateAllowedKeys ────────────────────────────────────────────────────────

func TestSSHServer_UpdateAllowedKeys_ReplacesExisting(t *testing.T) {
	hostDir := t.TempDir()
	clientDir1 := t.TempDir()
	clientDir2 := t.TempDir()

	_, err := GenerateSSHKeyPair(hostDir)
	require.NoError(t, err, "GenerateSSHKeyPair(host)")

	signer1, pub1 := generateTestKeyPair(t, clientDir1)
	_, pub2 := generateTestKeyPair(t, clientDir2)

	srv, addr, cancel := startTestServer(t, filepath.Join(hostDir, "id_ed25519"))
	defer cancel()

	// First: allow key1.
	srv.UpdateAllowedKeys(map[string]gossh.PublicKey{"dev1": pub1})

	client, err := dialSSH(t, addr, signer1)
	require.NoError(t, err, "dial with key1 (should succeed)")
	client.Close()

	// Now replace with only key2.
	srv.UpdateAllowedKeys(map[string]gossh.PublicKey{"dev2": pub2})

	// key1 should now be denied.
	_, err = dialSSH(t, addr, signer1)
	assert.Error(t, err, "dial with revoked key1 succeeded, expected failure")
}

// ─── publicKeyCallback ────────────────────────────────────────────────────────

func TestSSHServer_PublicKeyCallback_Allowed(t *testing.T) {
	dir := t.TempDir()
	_, err := GenerateSSHKeyPair(dir)
	require.NoError(t, err, "GenerateSSHKeyPair()")

	srv, err := NewSSHServer(0, filepath.Join(dir, "id_ed25519"), discardLogger())
	require.NoError(t, err, "NewSSHServer()")

	clientDir := t.TempDir()
	_, pub := generateTestKeyPair(t, clientDir)

	srv.UpdateAllowedKeys(map[string]gossh.PublicKey{"device-1": pub})

	perms, err := srv.publicKeyCallback(nil, pub)
	require.NoError(t, err, "publicKeyCallback() rejected allowed key")
	assert.NotNil(t, perms, "publicKeyCallback() returned nil Permissions, want non-nil")
}

func TestSSHServer_PublicKeyCallback_Denied(t *testing.T) {
	dir := t.TempDir()
	_, err := GenerateSSHKeyPair(dir)
	require.NoError(t, err, "GenerateSSHKeyPair()")

	srv, err := NewSSHServer(0, filepath.Join(dir, "id_ed25519"), discardLogger())
	require.NoError(t, err, "NewSSHServer()")

	// Generate a key but don't add it to allowed keys.
	clientDir := t.TempDir()
	_, pub := generateTestKeyPair(t, clientDir)

	_, err = srv.publicKeyCallback(nil, pub)
	require.Error(t, err, "publicKeyCallback() accepted unauthorized key, want error")
	assert.True(t, strings.Contains(err.Error(), "not authorized"),
		"publicKeyCallback() error = %q, want to contain \"not authorized\"", err.Error())
}

// ─── Stop ─────────────────────────────────────────────────────────────────────

func TestSSHServer_Stop_NilListenerIsNoOp(t *testing.T) {
	dir := t.TempDir()
	_, err := GenerateSSHKeyPair(dir)
	require.NoError(t, err, "GenerateSSHKeyPair()")

	srv, err := NewSSHServer(0, filepath.Join(dir, "id_ed25519"), discardLogger())
	require.NoError(t, err, "NewSSHServer()")

	// listener is nil, Stop should not panic or error.
	assert.NoError(t, srv.Stop(), "Stop() on nil listener")
}

// TestSSHServer_Stop_AfterContextCancel ensures Stop() is idempotent when
// Start's ctx-cancel goroutine has already closed the listener. This mirrors
// the real shutdown path: signal cancels ctx → listener closes → Daemon.Shutdown
// calls Stop again, and the second close must not surface as an error.
func TestSSHServer_Stop_AfterContextCancel(t *testing.T) {
	hostDir := t.TempDir()
	if _, err := GenerateSSHKeyPair(hostDir); err != nil {
		t.Fatalf("GenerateSSHKeyPair(): %v", err)
	}

	// Find a free port.
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("find free port: %v", err)
	}
	port := probe.Addr().(*net.TCPAddr).Port
	probe.Close()

	srv, err := NewSSHServer(port, filepath.Join(hostDir, "id_ed25519"), discardLogger())
	if err != nil {
		t.Fatalf("NewSSHServer(): %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Start(ctx) }()

	// Wait for the listener to be bound.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		srv.mu.RLock()
		ln := srv.listener
		srv.mu.RUnlock()
		if ln != nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Start() returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Start() did not return after ctx cancel")
	}

	// Second close (from Daemon.Shutdown) must not report net.ErrClosed.
	if err := srv.Stop(); err != nil {
		t.Errorf("Stop() after ctx-cancel close: %v", err)
	}
}

// ─── Integration: Start with port ─────────────────────────────────────────────

func TestSSHServer_StartListensOnPort(t *testing.T) {
	hostDir := t.TempDir()
	_, err := GenerateSSHKeyPair(hostDir)
	require.NoError(t, err, "GenerateSSHKeyPair()")

	// Find a free port.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err, "find free port")
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()

	srv, err := NewSSHServer(port, filepath.Join(hostDir, "id_ed25519"), discardLogger())
	require.NoError(t, err, "NewSSHServer()")

	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	go func() {
		close(started)
		_ = srv.Start(ctx)
	}()

	<-started
	// Give Start a moment to bind the port.
	time.Sleep(50 * time.Millisecond)

	// Verify port is reachable.
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 2*time.Second)
	if err != nil {
		cancel()
		t.Fatalf("dial started server on port %d: %v", port, err)
	}
	conn.Close()

	cancel()
}

// ─── device_id propagation via Permissions.Extensions ────────────────────────

func TestSSHServer_PublicKeyCallback_StampsDeviceID(t *testing.T) {
	dir := t.TempDir()
	_, err := GenerateSSHKeyPair(dir)
	require.NoError(t, err, "GenerateSSHKeyPair()")

	srv, err := NewSSHServer(0, filepath.Join(dir, "id_ed25519"), discardLogger())
	require.NoError(t, err, "NewSSHServer()")

	clientDir := t.TempDir()
	_, pub := generateTestKeyPair(t, clientDir)
	srv.UpdateAllowedKeys(map[string]gossh.PublicKey{"dev-bob": pub})

	perms, err := srv.publicKeyCallback(nil, pub)
	require.NoError(t, err, "publicKeyCallback()")
	require.NotNil(t, perms, "nil Permissions")
	assert.Equal(t, "dev-bob", perms.Extensions["hubfuse-device-id"],
		"device_id from UpdateAllowedKeys must be propagated via ssh.Permissions.Extensions")
}
