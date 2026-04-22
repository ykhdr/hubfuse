package scenarios_test

import (
	"bytes"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pkg/sftp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	gossh "golang.org/x/crypto/ssh"

	"github.com/ykhdr/hubfuse/tests/scenarios/helpers"
)

// dialSFTPAs opens a direct SFTP session from dialer -> peer using dialer's
// SSH private key (the one exchanged during pairing). Bypasses the mount CLI
// so ACL behaviour can be observed without involving stub-sshfs.
func dialSFTPAs(t *testing.T, dialer *helpers.Agent, peer *helpers.Agent) *sftp.Client {
	t.Helper()
	keyPath := filepath.Join(dialer.HomeDir, ".hubfuse", "keys", "id_ed25519")
	raw, err := os.ReadFile(keyPath)
	require.NoError(t, err, "read dialer ssh key")
	signer, err := gossh.ParsePrivateKey(raw)
	require.NoError(t, err, "parse dialer ssh key")

	cfg := &gossh.ClientConfig{
		User:            "hubfuse",
		Auth:            []gossh.AuthMethod{gossh.PublicKeys(signer)},
		HostKeyCallback: gossh.InsecureIgnoreHostKey(),
		Timeout:         5 * time.Second,
	}
	addr := net.JoinHostPort("127.0.0.1", fmt.Sprintf("%d", peer.SSHPort))
	sshClient, err := gossh.Dial("tcp", addr, cfg)
	require.NoError(t, err, "ssh dial %s", addr)
	t.Cleanup(func() { _ = sshClient.Close() })

	sftpClient, err := sftp.NewClient(sshClient)
	require.NoError(t, err, "sftp open")
	t.Cleanup(func() { _ = sftpClient.Close() })
	return sftpClient
}

// TestACL_ReadOnlyRejectsWrites — a share declared ro accepts reads and
// rejects writes from an allowed peer.
func TestACL_ReadOnlyRejectsWrites(t *testing.T) {
	hub := helpers.StartHub(t)
	exportDir := t.TempDir()
	require.NoError(t, writeTestFile(exportDir, "hello.txt", "hi"), "seed export")

	alice := helpers.StartAgent(t, hub, "alice",
		helpers.WithExportACL(exportDir, "docs", "ro", "bob"))
	alice.Join(t)
	alice.StartDaemon(t)

	bob := helpers.StartAgent(t, hub, "bob")
	bob.Join(t)
	bob.StartDaemon(t)

	require.Eventually(t,
		func() bool { return alice.HasPeer(t, "bob") && bob.HasPeer(t, "alice") },
		5*time.Second, 200*time.Millisecond, "hub should see both devices online")

	code := alice.RequestPairing(t, "bob")
	bob.ConfirmPairing(t, code)
	require.True(t, alice.WaitForPairedWith(t, 5*time.Second),
		"alice should have saved bob's public key")

	client := dialSFTPAs(t, bob, alice)

	// Read side works.
	f, err := client.Open("/docs/hello.txt")
	require.NoError(t, err, "bob should be able to open ro share")
	defer f.Close()
	var buf bytes.Buffer
	_, err = buf.ReadFrom(f)
	require.NoError(t, err)
	assert.Equal(t, "hi", buf.String())

	// Write side is rejected.
	_, err = client.Create("/docs/new.txt")
	assert.Error(t, err, "write to ro share must fail")
}
