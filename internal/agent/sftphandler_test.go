package agent

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/pkg/sftp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func mkACLHandlers(t *testing.T, deviceID string, acls []ShareACL, r DeviceResolver) *aclHandlers {
	t.Helper()
	return newACLHandlers(deviceID, r, func() []ShareACL { return acls }, discardLogger())
}

func newRequest(method, filepath_ string) *sftp.Request {
	return sftp.NewRequest(method, filepath_)
}

func TestACLHandlers_ListRoot_ShowsOnlyAllowedShares(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "hello.txt"), []byte("hi"), 0o644))

	acls := []ShareACL{
		{Alias: "visible", Path: dir, AllowAll: true, ReadOnly: true},
		{Alias: "hidden", Path: dir, AllowedDevices: []string{"carol"}, ReadOnly: true},
	}
	h := mkACLHandlers(t, "dev-bob", acls, stubResolver{"dev-bob": "bob"})

	lister, err := h.Filelist(newRequest("List", "/"))
	require.NoError(t, err)

	buf := make([]os.FileInfo, 8)
	n, err := lister.ListAt(buf, 0)
	if err != nil && err != io.EOF {
		t.Fatalf("ListAt: %v", err)
	}
	names := make([]string, n)
	for i := 0; i < n; i++ {
		names[i] = buf[i].Name()
	}
	assert.ElementsMatch(t, []string{"visible"}, names)
}

func TestACLHandlers_Filewrite_ReadOnlyDenies(t *testing.T) {
	dir := t.TempDir()
	acls := []ShareACL{{Alias: "docs", Path: dir, AllowAll: true, ReadOnly: true}}
	h := mkACLHandlers(t, "dev-bob", acls, stubResolver{})

	_, err := h.Filewrite(newRequest("Put", "/docs/new.txt"))
	assert.ErrorIs(t, err, sftp.ErrSSHFxPermissionDenied)
}

func TestACLHandlers_Filewrite_RW_Allows(t *testing.T) {
	dir := t.TempDir()
	acls := []ShareACL{{Alias: "docs", Path: dir, AllowAll: true, ReadOnly: false}}
	h := mkACLHandlers(t, "dev-bob", acls, stubResolver{})

	w, err := h.Filewrite(newRequest("Put", "/docs/new.txt"))
	require.NoError(t, err)
	require.NotNil(t, w)
	if c, ok := w.(io.Closer); ok {
		_ = c.Close()
	}
	_, statErr := os.Stat(filepath.Join(dir, "new.txt"))
	assert.NoError(t, statErr)
}

func TestACLHandlers_Fileread_UnknownShareDenied(t *testing.T) {
	h := mkACLHandlers(t, "dev-bob", nil, stubResolver{})
	_, err := h.Fileread(newRequest("Get", "/missing/file"))
	assert.ErrorIs(t, err, sftp.ErrSSHFxPermissionDenied)
}

func TestACLHandlers_Fileread_ShareNotAllowedDenied(t *testing.T) {
	dir := t.TempDir()
	acls := []ShareACL{{Alias: "docs", Path: dir, AllowedDevices: []string{"carol"}, ReadOnly: true}}
	h := mkACLHandlers(t, "dev-bob", acls, stubResolver{"dev-bob": "bob"})
	_, err := h.Fileread(newRequest("Get", "/docs/x"))
	assert.ErrorIs(t, err, sftp.ErrSSHFxPermissionDenied)
}

func TestACLHandlers_Filecmd_WriteClassAgainstRO(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "old.txt"), nil, 0o644))
	acls := []ShareACL{{Alias: "docs", Path: dir, AllowAll: true, ReadOnly: true}}
	h := mkACLHandlers(t, "dev-bob", acls, stubResolver{})

	for _, m := range []string{"Setstat", "Rename", "Remove", "Rmdir", "Mkdir", "Symlink", "Link"} {
		err := h.Filecmd(newRequest(m, "/docs/old.txt"))
		assert.ErrorIs(t, err, sftp.ErrSSHFxPermissionDenied, "method %s on ro share", m)
	}
}

func TestACLHandlers_Fileread_SymlinkEscapeDenied(t *testing.T) {
	outside := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("top secret"), 0o644))

	shareDir := t.TempDir()
	// Plant a symlink inside the share pointing to a file outside it.
	require.NoError(t, os.Symlink(filepath.Join(outside, "secret.txt"),
		filepath.Join(shareDir, "escape")))

	acls := []ShareACL{{Alias: "docs", Path: shareDir, AllowAll: true, ReadOnly: true}}
	h := mkACLHandlers(t, "dev-bob", acls, stubResolver{})

	_, err := h.Fileread(newRequest("Get", "/docs/escape"))
	assert.ErrorIs(t, err, sftp.ErrSSHFxPermissionDenied,
		"reading through a symlink that escapes the share must be denied")
}

func TestACLHandlers_Filecmd_SymlinkAlwaysDenied(t *testing.T) {
	dir := t.TempDir()
	// Even a read-write share must refuse Symlink creation: letting a peer
	// plant arbitrary symlinks would reopen the escape hole resolveReadReal
	// exists to close.
	acls := []ShareACL{{Alias: "docs", Path: dir, AllowAll: true, ReadOnly: false}}
	h := mkACLHandlers(t, "dev-bob", acls, stubResolver{})

	req := newRequest("Symlink", "/docs/link")
	req.Target = "/etc/passwd"
	err := h.Filecmd(req)
	assert.ErrorIs(t, err, sftp.ErrSSHFxPermissionDenied)
}

// TestACLHandlers_Filelist_Lstat_NonexistentLeaf checks that stat of a path
// that does not exist inside an allowed share returns SSH_FX_NO_SUCH_FILE, not
// PERMISSION_DENIED. sshfs relies on this to cache negative dentries before
// create/mkdir (issue #46).
func TestACLHandlers_Filelist_Lstat_NonexistentLeaf(t *testing.T) {
	dir := t.TempDir()
	acls := []ShareACL{{Alias: "docs", Path: dir, AllowAll: true, ReadOnly: true}}
	h := mkACLHandlers(t, "dev-bob", acls, stubResolver{})

	_, err := h.Filelist(newRequest("Lstat", "/docs/nonexistent.txt"))
	assert.ErrorIs(t, err, sftp.ErrSSHFxNoSuchFile)
}

// TestACLHandlers_Filelist_Stat_NonexistentLeaf is the Stat variant of the
// above (sshfs uses both Lstat and Stat during kernel lookups).
func TestACLHandlers_Filelist_Stat_NonexistentLeaf(t *testing.T) {
	dir := t.TempDir()
	acls := []ShareACL{{Alias: "docs", Path: dir, AllowAll: true, ReadOnly: true}}
	h := mkACLHandlers(t, "dev-bob", acls, stubResolver{})

	_, err := h.Filelist(newRequest("Stat", "/docs/nonexistent.txt"))
	assert.ErrorIs(t, err, sftp.ErrSSHFxNoSuchFile)
}

// TestACLHandlers_Filelist_Stat_NonexistentIntermediateDir verifies that a path
// with a nonexistent intermediate directory also returns SSH_FX_NO_SUCH_FILE.
// filepath.EvalSymlinks fails at the first missing component, so both leaf and
// intermediate missing cases go through the same code path.
func TestACLHandlers_Filelist_Stat_NonexistentIntermediateDir(t *testing.T) {
	dir := t.TempDir()
	acls := []ShareACL{{Alias: "docs", Path: dir, AllowAll: true, ReadOnly: true}}
	h := mkACLHandlers(t, "dev-bob", acls, stubResolver{})

	_, err := h.Filelist(newRequest("Stat", "/docs/sub/x"))
	assert.ErrorIs(t, err, sftp.ErrSSHFxNoSuchFile)
}

// TestACLHandlers_Fileread_NonexistentFile checks that reading a file that does
// not exist inside an allowed share returns SSH_FX_NO_SUCH_FILE (issue #46).
func TestACLHandlers_Fileread_NonexistentFile(t *testing.T) {
	dir := t.TempDir()
	acls := []ShareACL{{Alias: "docs", Path: dir, AllowAll: true, ReadOnly: true}}
	h := mkACLHandlers(t, "dev-bob", acls, stubResolver{})

	_, err := h.Fileread(newRequest("Get", "/docs/missing.txt"))
	assert.ErrorIs(t, err, sftp.ErrSSHFxNoSuchFile)
}

// TestACLHandlers_Filecmd_Mkdir_NonexistentParent verifies that mkdir under a
// nonexistent parent directory returns SSH_FX_NO_SUCH_FILE so the client knows
// to create the parent first rather than receiving PERMISSION_DENIED (issue #46).
func TestACLHandlers_Filecmd_Mkdir_NonexistentParent(t *testing.T) {
	dir := t.TempDir()
	acls := []ShareACL{{Alias: "docs", Path: dir, AllowAll: true, ReadOnly: false}}
	h := mkACLHandlers(t, "dev-bob", acls, stubResolver{})

	err := h.Filecmd(newRequest("Mkdir", "/docs/missing/newdir"))
	assert.ErrorIs(t, err, sftp.ErrSSHFxNoSuchFile)
}

// TestACLHandlers_Filelist_Stat_DanglingSymlink documents the accepted trade-off:
// a dangling symlink (symlink whose target does not exist) reports
// SSH_FX_NO_SUCH_FILE. This is POSIX-honest — POSIX stat(2) of a dangling
// symlink returns ENOENT — and safe because peers cannot plant symlinks
// (Filecmd rejects the Symlink method unconditionally).
func TestACLHandlers_Filelist_Stat_DanglingSymlink(t *testing.T) {
	dir := t.TempDir()
	// Create a symlink inside the share pointing to a nonexistent target.
	require.NoError(t, os.Symlink(filepath.Join(dir, "ghost.txt"),
		filepath.Join(dir, "dangling")))

	acls := []ShareACL{{Alias: "docs", Path: dir, AllowAll: true, ReadOnly: true}}
	h := mkACLHandlers(t, "dev-bob", acls, stubResolver{})

	// Stat (follows symlinks) of a dangling symlink → ENOENT → SSH_FX_NO_SUCH_FILE.
	_, err := h.Filelist(newRequest("Stat", "/docs/dangling"))
	assert.ErrorIs(t, err, sftp.ErrSSHFxNoSuchFile)
}

// TestACLHandlers_Filelist_Stat_ExistingFile is a regression guard: stat of an
// existing file in an allowed share must still succeed.
func TestACLHandlers_Filelist_Stat_ExistingFile(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "hello.txt"), []byte("hi"), 0o644))

	acls := []ShareACL{{Alias: "docs", Path: dir, AllowAll: true, ReadOnly: true}}
	h := mkACLHandlers(t, "dev-bob", acls, stubResolver{})

	lister, err := h.Filelist(newRequest("Stat", "/docs/hello.txt"))
	require.NoError(t, err)
	require.NotNil(t, lister)
}
