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

// TestACL_AllowedDevicesFiltersListing — alice exports a share that names
// only bob in allowed-devices. bob sees it in the synthetic root listing and
// can read; carol does not see it and is denied on direct access.
func TestACL_AllowedDevicesFiltersListing(t *testing.T) {
	hub := helpers.StartHub(t)
	exportDir := t.TempDir()
	require.NoError(t, writeTestFile(exportDir, "secret.txt", "s3cr3t"), "seed export")

	alice := helpers.StartAgent(t, hub, "alice",
		helpers.WithExportACL(exportDir, "docs", "ro", "bob"))
	alice.Join(t)
	alice.StartDaemon(t)

	bob := helpers.StartAgent(t, hub, "bob")
	bob.Join(t)
	bob.StartDaemon(t)

	carol := helpers.StartAgent(t, hub, "carol")
	carol.Join(t)
	carol.StartDaemon(t)

	require.Eventually(t,
		func() bool { return alice.HasPeer(t, "bob") && alice.HasPeer(t, "carol") },
		5*time.Second, 200*time.Millisecond, "hub should see both peers")

	bobCode := alice.RequestPairing(t, "bob")
	bob.ConfirmPairing(t, bobCode)
	require.True(t, alice.WaitForPairedCount(t, 1, 5*time.Second),
		"alice should have saved bob's public key")

	carolCode := alice.RequestPairing(t, "carol")
	carol.ConfirmPairing(t, carolCode)
	require.True(t, alice.WaitForPairedCount(t, 2, 5*time.Second),
		"alice should have saved carol's public key")

	// bob — share visible, read works.
	bobClient := dialSFTPAs(t, bob, alice)
	bobEntries, err := bobClient.ReadDir("/")
	require.NoError(t, err)
	var bobNames []string
	for _, e := range bobEntries {
		bobNames = append(bobNames, e.Name())
	}
	assert.Contains(t, bobNames, "docs", "bob should see docs in the root listing")

	// carol — share must not appear; direct access denied.
	carolClient := dialSFTPAs(t, carol, alice)
	carolEntries, err := carolClient.ReadDir("/")
	require.NoError(t, err, "root listing itself should succeed for carol")
	for _, e := range carolEntries {
		assert.NotEqual(t, "docs", e.Name(), "carol must not see docs")
	}
	_, err = carolClient.Open("/docs/secret.txt")
	assert.Error(t, err, "direct access by carol must be denied")
}
