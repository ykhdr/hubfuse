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
	gossh "golang.org/x/crypto/ssh"
)

// generateTestKeyPair generates an SSH key pair in dir and returns the
// private key signer and the parsed public key.
func generateTestKeyPair(t *testing.T, dir string) (gossh.Signer, gossh.PublicKey) {
	t.Helper()
	_, err := GenerateSSHKeyPair(dir)
	if err != nil {
		t.Fatalf("GenerateSSHKeyPair(): %v", err)
	}

	privBytes, err := os.ReadFile(filepath.Join(dir, "id_ed25519"))
	if err != nil {
		t.Fatalf("read private key: %v", err)
	}

	signer, err := gossh.ParsePrivateKey(privBytes)
	if err != nil {
		t.Fatalf("parse private key: %v", err)
	}

	pubBytes, err := os.ReadFile(filepath.Join(dir, "id_ed25519.pub"))
	if err != nil {
		t.Fatalf("read public key: %v", err)
	}

	pub, _, _, _, err := gossh.ParseAuthorizedKey(pubBytes)
	if err != nil {
		t.Fatalf("parse public key: %v", err)
	}

	return signer, pub
}

// startTestServer starts an SSHServer on a random port and returns the
// server, its address, and a cancel function.
func startTestServer(t *testing.T, hostKeyPath string) (*SSHServer, string, context.CancelFunc) {
	t.Helper()

	srv, err := NewSSHServer(0, hostKeyPath, discardLogger())
	if err != nil {
		t.Fatalf("NewSSHServer(): %v", err)
	}

	// Use port 0 to get a random port.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen(): %v", err)
	}
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
	if err == nil {
		t.Fatal("NewSSHServer() expected error for missing host key, got nil")
	}
}

func TestNewSSHServer_InvalidHostKey(t *testing.T) {
	dir := t.TempDir()
	badKey := filepath.Join(dir, "bad_key")
	if err := os.WriteFile(badKey, []byte("not a valid pem key"), 0600); err != nil {
		t.Fatalf("WriteFile(): %v", err)
	}

	_, err := NewSSHServer(2222, badKey, discardLogger())
	if err == nil {
		t.Fatal("NewSSHServer() expected error for invalid host key, got nil")
	}
}

func TestNewSSHServer_ValidHostKey(t *testing.T) {
	dir := t.TempDir()
	if _, err := GenerateSSHKeyPair(dir); err != nil {
		t.Fatalf("GenerateSSHKeyPair(): %v", err)
	}

	hostKeyPath := filepath.Join(dir, "id_ed25519")
	_, err := NewSSHServer(2222, hostKeyPath, discardLogger())
	if err != nil {
		t.Fatalf("NewSSHServer(): %v", err)
	}
}

// ─── Start / Stop ─────────────────────────────────────────────────────────────

func TestSSHServer_StartStop(t *testing.T) {
	dir := t.TempDir()
	if _, err := GenerateSSHKeyPair(dir); err != nil {
		t.Fatalf("GenerateSSHKeyPair(): %v", err)
	}

	hostKeyPath := filepath.Join(dir, "id_ed25519")
	srv, err := NewSSHServer(0, hostKeyPath, discardLogger())
	if err != nil {
		t.Fatalf("NewSSHServer(): %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen(): %v", err)
	}
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
	if err != nil {
		t.Fatalf("dial server: %v", err)
	}
	conn.Close()

	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("server exited with error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Error("server did not stop within timeout")
	}
}

// ─── Public key authentication ────────────────────────────────────────────────

func TestSSHServer_AllowedKeyAuthentication(t *testing.T) {
	hostDir := t.TempDir()
	clientDir := t.TempDir()

	if _, err := GenerateSSHKeyPair(hostDir); err != nil {
		t.Fatalf("GenerateSSHKeyPair(host): %v", err)
	}

	clientSigner, clientPub := generateTestKeyPair(t, clientDir)

	srv, addr, cancel := startTestServer(t, filepath.Join(hostDir, "id_ed25519"))
	defer cancel()

	// Allow the client key.
	srv.UpdateAllowedKeys(map[string]gossh.PublicKey{
		"client-device": clientPub,
	})

	client, err := dialSSH(t, addr, clientSigner)
	if err != nil {
		t.Fatalf("SSH dial with allowed key: %v", err)
	}
	defer client.Close()
}

func TestSSHServer_DeniedKeyAuthentication(t *testing.T) {
	hostDir := t.TempDir()
	allowedDir := t.TempDir()
	deniedDir := t.TempDir()

	if _, err := GenerateSSHKeyPair(hostDir); err != nil {
		t.Fatalf("GenerateSSHKeyPair(host): %v", err)
	}

	_, allowedPub := generateTestKeyPair(t, allowedDir)
	deniedSigner, _ := generateTestKeyPair(t, deniedDir)

	srv, addr, cancel := startTestServer(t, filepath.Join(hostDir, "id_ed25519"))
	defer cancel()

	// Only allow the allowedPub key, not the deniedSigner key.
	srv.UpdateAllowedKeys(map[string]gossh.PublicKey{
		"allowed-device": allowedPub,
	})

	_, err := dialSSH(t, addr, deniedSigner)
	if err == nil {
		t.Fatal("SSH dial with denied key succeeded, expected failure")
	}
}

func TestSSHServer_NoAllowedKeys(t *testing.T) {
	hostDir := t.TempDir()
	clientDir := t.TempDir()

	if _, err := GenerateSSHKeyPair(hostDir); err != nil {
		t.Fatalf("GenerateSSHKeyPair(host): %v", err)
	}

	clientSigner, _ := generateTestKeyPair(t, clientDir)

	srv, addr, cancel := startTestServer(t, filepath.Join(hostDir, "id_ed25519"))
	defer cancel()

	// No keys allowed.
	srv.UpdateAllowedKeys(map[string]gossh.PublicKey{})

	_, err := dialSSH(t, addr, clientSigner)
	if err == nil {
		t.Fatal("SSH dial with no allowed keys succeeded, expected failure")
	}
}

// ─── UpdateShares / SFTP alias mapping ───────────────────────────────────────

func TestSSHServer_UpdateShares_RebuildsSFTPRoot(t *testing.T) {
	hostDir := t.TempDir()
	if _, err := GenerateSSHKeyPair(hostDir); err != nil {
		t.Fatalf("GenerateSSHKeyPair(host): %v", err)
	}

	srv, err := NewSSHServer(0, filepath.Join(hostDir, "id_ed25519"), discardLogger())
	if err != nil {
		t.Fatalf("NewSSHServer(): %v", err)
	}

	// Create a real directory to be shared.
	sharedDir := t.TempDir()
	testFile := filepath.Join(sharedDir, "hello.txt")
	if err := os.WriteFile(testFile, []byte("hello"), 0644); err != nil {
		t.Fatalf("WriteFile(): %v", err)
	}

	shares := map[string]string{"myalias": sharedDir}
	srv.UpdateShares(shares)

	// Verify symlink was created.
	linkPath := filepath.Join(srv.sftpRoot, "myalias")
	info, err := os.Lstat(linkPath)
	if err != nil {
		t.Fatalf("Lstat(%q): %v", linkPath, err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Errorf("%q is not a symlink", linkPath)
	}

	// Resolve and verify target.
	target, err := os.Readlink(linkPath)
	if err != nil {
		t.Fatalf("Readlink(%q): %v", linkPath, err)
	}
	if target != sharedDir {
		t.Errorf("symlink target = %q, want %q", target, sharedDir)
	}
}

func TestSSHServer_UpdateShares_ReplacesOldSymlinks(t *testing.T) {
	hostDir := t.TempDir()
	if _, err := GenerateSSHKeyPair(hostDir); err != nil {
		t.Fatalf("GenerateSSHKeyPair(host): %v", err)
	}

	srv, err := NewSSHServer(0, filepath.Join(hostDir, "id_ed25519"), discardLogger())
	if err != nil {
		t.Fatalf("NewSSHServer(): %v", err)
	}

	dir1 := t.TempDir()
	dir2 := t.TempDir()

	srv.UpdateShares(map[string]string{"alias-old": dir1})
	srv.UpdateShares(map[string]string{"alias-new": dir2})

	// Old symlink should be gone.
	if _, err := os.Lstat(filepath.Join(srv.sftpRoot, "alias-old")); err == nil {
		t.Error("old symlink still exists after UpdateShares")
	}

	// New symlink should exist.
	if _, err := os.Lstat(filepath.Join(srv.sftpRoot, "alias-new")); err != nil {
		t.Errorf("new symlink not found: %v", err)
	}
}

// ─── SFTP file access ─────────────────────────────────────────────────────────

func TestSSHServer_SFTPFileAccess(t *testing.T) {
	hostDir := t.TempDir()
	clientDir := t.TempDir()
	sharedDir := t.TempDir()

	if _, err := GenerateSSHKeyPair(hostDir); err != nil {
		t.Fatalf("GenerateSSHKeyPair(host): %v", err)
	}
	clientSigner, clientPub := generateTestKeyPair(t, clientDir)

	// Write a test file into the shared directory.
	want := "sftp-test-content"
	testFile := filepath.Join(sharedDir, "test.txt")
	if err := os.WriteFile(testFile, []byte(want), 0644); err != nil {
		t.Fatalf("WriteFile(): %v", err)
	}

	srv, addr, cancel := startTestServer(t, filepath.Join(hostDir, "id_ed25519"))
	defer cancel()

	srv.UpdateAllowedKeys(map[string]gossh.PublicKey{"client": clientPub})
	srv.UpdateShares(map[string]string{"myshare": sharedDir})

	// Dial SSH.
	sshClient, err := dialSSH(t, addr, clientSigner)
	if err != nil {
		t.Fatalf("SSH dial: %v", err)
	}
	defer sshClient.Close()

	// Open SFTP session.
	sftpClient, err := sftp.NewClient(sshClient)
	if err != nil {
		t.Fatalf("sftp.NewClient(): %v", err)
	}
	defer sftpClient.Close()

	// Read a file through the alias symlink path.
	aliasFilePath := filepath.Join(srv.sftpRoot, "myshare", "test.txt")
	f, err := sftpClient.Open(aliasFilePath)
	if err != nil {
		t.Fatalf("sftp.Open(%q): %v", aliasFilePath, err)
	}
	defer f.Close()

	data, err := io.ReadAll(f)
	if err != nil {
		t.Fatalf("io.ReadAll(): %v", err)
	}

	if string(data) != want {
		t.Errorf("file contents = %q, want %q", string(data), want)
	}
}

// ─── UpdateAllowedKeys ────────────────────────────────────────────────────────

func TestSSHServer_UpdateAllowedKeys_ReplacesExisting(t *testing.T) {
	hostDir := t.TempDir()
	clientDir1 := t.TempDir()
	clientDir2 := t.TempDir()

	if _, err := GenerateSSHKeyPair(hostDir); err != nil {
		t.Fatalf("GenerateSSHKeyPair(host): %v", err)
	}

	signer1, pub1 := generateTestKeyPair(t, clientDir1)
	_, pub2 := generateTestKeyPair(t, clientDir2)

	srv, addr, cancel := startTestServer(t, filepath.Join(hostDir, "id_ed25519"))
	defer cancel()

	// First: allow key1.
	srv.UpdateAllowedKeys(map[string]gossh.PublicKey{"dev1": pub1})

	client, err := dialSSH(t, addr, signer1)
	if err != nil {
		t.Fatalf("dial with key1 (should succeed): %v", err)
	}
	client.Close()

	// Now replace with only key2.
	srv.UpdateAllowedKeys(map[string]gossh.PublicKey{"dev2": pub2})

	// key1 should now be denied.
	_, err = dialSSH(t, addr, signer1)
	if err == nil {
		t.Fatal("dial with revoked key1 succeeded, expected failure")
	}
}

// ─── publicKeyCallback ────────────────────────────────────────────────────────

func TestSSHServer_PublicKeyCallback_Allowed(t *testing.T) {
	dir := t.TempDir()
	if _, err := GenerateSSHKeyPair(dir); err != nil {
		t.Fatalf("GenerateSSHKeyPair(): %v", err)
	}

	srv, err := NewSSHServer(0, filepath.Join(dir, "id_ed25519"), discardLogger())
	if err != nil {
		t.Fatalf("NewSSHServer(): %v", err)
	}

	clientDir := t.TempDir()
	_, pub := generateTestKeyPair(t, clientDir)

	srv.UpdateAllowedKeys(map[string]gossh.PublicKey{"device-1": pub})

	perms, err := srv.publicKeyCallback(nil, pub)
	if err != nil {
		t.Fatalf("publicKeyCallback() rejected allowed key: %v", err)
	}
	if perms == nil {
		t.Error("publicKeyCallback() returned nil Permissions, want non-nil")
	}
}

func TestSSHServer_PublicKeyCallback_Denied(t *testing.T) {
	dir := t.TempDir()
	if _, err := GenerateSSHKeyPair(dir); err != nil {
		t.Fatalf("GenerateSSHKeyPair(): %v", err)
	}

	srv, err := NewSSHServer(0, filepath.Join(dir, "id_ed25519"), discardLogger())
	if err != nil {
		t.Fatalf("NewSSHServer(): %v", err)
	}

	// Generate a key but don't add it to allowed keys.
	clientDir := t.TempDir()
	_, pub := generateTestKeyPair(t, clientDir)

	_, err = srv.publicKeyCallback(nil, pub)
	if err == nil {
		t.Fatal("publicKeyCallback() accepted unauthorized key, want error")
	}
	if !strings.Contains(err.Error(), "not authorized") {
		t.Errorf("publicKeyCallback() error = %q, want to contain \"not authorized\"", err.Error())
	}
}

// ─── Stop ─────────────────────────────────────────────────────────────────────

func TestSSHServer_Stop_NilListenerIsNoOp(t *testing.T) {
	dir := t.TempDir()
	if _, err := GenerateSSHKeyPair(dir); err != nil {
		t.Fatalf("GenerateSSHKeyPair(): %v", err)
	}

	srv, err := NewSSHServer(0, filepath.Join(dir, "id_ed25519"), discardLogger())
	if err != nil {
		t.Fatalf("NewSSHServer(): %v", err)
	}

	// listener is nil, Stop should not panic or error.
	if err := srv.Stop(); err != nil {
		t.Errorf("Stop() on nil listener: %v", err)
	}
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
	if _, err := GenerateSSHKeyPair(hostDir); err != nil {
		t.Fatalf("GenerateSSHKeyPair(): %v", err)
	}

	// Find a free port.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("find free port: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()

	srv, err := NewSSHServer(port, filepath.Join(hostDir, "id_ed25519"), discardLogger())
	if err != nil {
		t.Fatalf("NewSSHServer(): %v", err)
	}

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
