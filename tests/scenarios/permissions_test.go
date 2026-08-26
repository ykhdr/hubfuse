package scenarios_test

import (
	"bytes"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

// shareVisibleTimeout bounds the wait in waitForShare.
//
// The chain being waited on is long and entirely asynchronous: `hubfuse share
// add` rewrites config.kdl, fsnotify delivers the change, the daemon reloads,
// and only then does the SSH server's alias→path map carry the new share. On an
// idle machine that settles well inside a second; on a shared CI runner it does
// not always fit in ten, which is how this timed out on a PR that touched
// nothing but whitespace (issue #85). Thirty seconds costs nothing when the
// wait succeeds — it returns as soon as the alias appears — and it is far below
// the package's own timeout, so a genuine hang still fails as this assertion
// rather than as a package-wide panic.
const shareVisibleTimeout = 30 * time.Second

// waitForShare blocks until alias appears in the peer's synthetic SFTP root.
// "share add" only rewrites config.kdl; the daemon applies it asynchronously
// when the config watcher fires, so a listing taken immediately after pairing
// can race the reload and come back empty.
//
// The failure message reports the last thing actually observed, because a bare
// timeout cannot distinguish the two failures that reach it: a slow runner
// (the listing worked and simply did not contain the alias yet) from a broken
// reload or a dead SFTP session (the listing itself errored). Those need
// opposite responses, and without this a maintainer can only tell them apart by
// re-running CI — which is exactly what issue #85 cost.
func waitForShare(t *testing.T, client *sftp.Client, alias string) {
	t.Helper()

	var (
		mu       sync.Mutex
		lastErr  error
		lastSeen []string
	)

	require.Eventuallyf(t, func() bool {
		entries, err := client.ReadDir("/")

		mu.Lock()
		lastErr = err
		lastSeen = nil
		for _, e := range entries {
			lastSeen = append(lastSeen, e.Name())
		}
		mu.Unlock()

		if err != nil {
			return false
		}
		for _, e := range entries {
			if e.Name() == alias {
				return true
			}
		}
		return false
	}, shareVisibleTimeout, 100*time.Millisecond,
		"share %q never appeared in the SFTP root listing within %s; last listing: %v, last error: %v",
		alias, shareVisibleTimeout, &deferredStrings{mu: &mu, v: &lastSeen}, &deferredError{mu: &mu, v: &lastErr})
}

// deferredStrings and deferredError defer reading the observation until the
// message is actually formatted, which happens on the polling goroutine's peer
// after Eventually has given up. Passing lastSeen/lastErr directly would
// evaluate them at CALL time — before a single poll has run — so the message
// would always report an empty listing and a nil error, which is worse than no
// message at all: it would look like evidence.
type deferredStrings struct {
	mu *sync.Mutex
	v  *[]string
}

func (d *deferredStrings) String() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(*d.v) == 0 {
		return "(empty)"
	}
	return strings.Join(*d.v, ", ")
}

type deferredError struct {
	mu *sync.Mutex
	v  *error
}

func (d *deferredError) String() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	if *d.v == nil {
		return "none"
	}
	return (*d.v).Error()
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
	waitForShare(t, client, "docs")

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

	// bob — share visible, read works. waitForShare also guarantees the
	// daemon has loaded the share before carol's must-not-see check below,
	// so that negative assertion cannot pass vacuously.
	bobClient := dialSFTPAs(t, bob, alice)
	waitForShare(t, bobClient, "docs")
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

// TestACL_WildcardAllowsEverybody — allowed-devices "all" means any paired peer.
func TestACL_WildcardAllowsEverybody(t *testing.T) {
	hub := helpers.StartHub(t)
	exportDir := t.TempDir()
	require.NoError(t, writeTestFile(exportDir, "pub.txt", "public"), "seed export")

	alice := helpers.StartAgent(t, hub, "alice",
		helpers.WithExportACL(exportDir, "pub", "ro", "all"))
	alice.Join(t)
	alice.StartDaemon(t)

	bob := helpers.StartAgent(t, hub, "bob")
	bob.Join(t)
	bob.StartDaemon(t)

	require.Eventually(t,
		func() bool { return alice.HasPeer(t, "bob") },
		5*time.Second, 200*time.Millisecond)

	code := alice.RequestPairing(t, "bob")
	bob.ConfirmPairing(t, code)
	require.True(t, alice.WaitForPairedWith(t, 5*time.Second))

	client := dialSFTPAs(t, bob, alice)
	waitForShare(t, client, "pub")
	entries, err := client.ReadDir("/")
	require.NoError(t, err)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	assert.Contains(t, names, "pub", `"all" wildcard must show share to paired peer`)
}

// TestACL_DefaultDeny — a share with no allowed-devices is invisible and
// inaccessible to every paired peer.
func TestACL_DefaultDeny(t *testing.T) {
	hub := helpers.StartHub(t)
	exportDir := t.TempDir()
	require.NoError(t, writeTestFile(exportDir, "private.txt", "nope"), "seed export")

	// Export with no --allow at all: permissions default to ro, AllowedDevices empty.
	alice := helpers.StartAgent(t, hub, "alice",
		helpers.WithExportACL(exportDir, "private", "ro"))
	alice.Join(t)
	alice.StartDaemon(t)

	bob := helpers.StartAgent(t, hub, "bob")
	bob.Join(t)
	bob.StartDaemon(t)

	require.Eventually(t,
		func() bool { return alice.HasPeer(t, "bob") },
		5*time.Second, 200*time.Millisecond)

	code := alice.RequestPairing(t, "bob")
	bob.ConfirmPairing(t, code)
	require.True(t, alice.WaitForPairedWith(t, 5*time.Second))

	client := dialSFTPAs(t, bob, alice)
	entries, err := client.ReadDir("/")
	require.NoError(t, err, "root listing itself should succeed, just empty")
	for _, e := range entries {
		assert.NotEqual(t, "private", e.Name(), "default-deny share must not appear")
	}
	_, err = client.Open("/private/private.txt")
	assert.Error(t, err, "direct access to default-deny share must be rejected")
}
