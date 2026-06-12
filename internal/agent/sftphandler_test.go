package agent

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/pkg/sftp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// SFTP open pflag bit constants (SSH_FXF_* values from the SFTP protocol spec).
const (
	pflagRead   uint32 = 0x1
	pflagWrite  uint32 = 0x2
	pflagAppend uint32 = 0x4
	pflagCreat  uint32 = 0x8
	pflagTrunc  uint32 = 0x10
	pflagExcl   uint32 = 0x20
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

// rwShare returns a single rw share ACL for the given directory.
func rwShare(dir string) []ShareACL {
	return []ShareACL{{Alias: "docs", Path: dir, AllowAll: true, ReadOnly: false}}
}

// mustCloseWriter asserts that the writer implements io.Closer and closes it.
func mustCloseWriter(t *testing.T, w io.WriterAt) {
	t.Helper()
	c, ok := w.(io.Closer)
	require.True(t, ok, "writer must implement io.Closer")
	require.NoError(t, c.Close())
}

// writeReq creates a Put request with the given raw pflag bits set.
func writeReq(path string, flags uint32) *sftp.Request {
	req := sftp.NewRequest("Put", path)
	req.Flags = flags
	return req
}

// TestACLHandlers_Filewrite_RplusPreservesPrefix verifies that a READ|WRITE open
// (pflags 0x3) of an existing file does not truncate it: writing "BBB\n" at
// offset 4 must preserve the original "AAA\n" prefix, leaving "AAA\nBBB\n".
// Bug #45: the old fallback added O_CREATE|O_TRUNC, producing "\0\0\0\0BBB\n".
func TestACLHandlers_Filewrite_RplusPreservesPrefix(t *testing.T) {
	dir := t.TempDir()
	fpath := filepath.Join(dir, "file.txt")
	require.NoError(t, os.WriteFile(fpath, []byte("AAA\n"), 0o644))

	h := mkACLHandlers(t, "dev-bob", rwShare(dir), stubResolver{})
	w, err := h.Filewrite(writeReq("/docs/file.txt", pflagRead|pflagWrite))
	require.NoError(t, err)

	_, err = w.WriteAt([]byte("BBB\n"), 4)
	require.NoError(t, err)
	mustCloseWriter(t, w)

	got, err := os.ReadFile(fpath)
	require.NoError(t, err)
	assert.Equal(t, "AAA\nBBB\n", string(got))
}

// TestACLHandlers_Filewrite_WriteOnlyPreservesPrefix verifies that a WRITE-only
// open (pflags 0x2) of an existing file does not truncate it.
func TestACLHandlers_Filewrite_WriteOnlyPreservesPrefix(t *testing.T) {
	dir := t.TempDir()
	fpath := filepath.Join(dir, "file.txt")
	require.NoError(t, os.WriteFile(fpath, []byte("AAA\n"), 0o644))

	h := mkACLHandlers(t, "dev-bob", rwShare(dir), stubResolver{})
	w, err := h.Filewrite(writeReq("/docs/file.txt", pflagWrite))
	require.NoError(t, err)

	_, err = w.WriteAt([]byte("BBB\n"), 4)
	require.NoError(t, err)
	mustCloseWriter(t, w)

	got, err := os.ReadFile(fpath)
	require.NoError(t, err)
	assert.Equal(t, "AAA\nBBB\n", string(got))
}

// TestACLHandlers_Filewrite_AppendOffsetCorrectClient verifies the append flow
// with a correct-offset client (like sshfs): WRITE|CREAT|APPEND with
// WriteAt("BBB\n", 4) must not error and must append to the seeded "AAA\n".
// Bug #54: the old code called os.File.WriteAt on an O_APPEND handle, which
// always returns an error.
func TestACLHandlers_Filewrite_AppendOffsetCorrectClient(t *testing.T) {
	dir := t.TempDir()
	fpath := filepath.Join(dir, "file.txt")
	require.NoError(t, os.WriteFile(fpath, []byte("AAA\n"), 0o644))

	h := mkACLHandlers(t, "dev-bob", rwShare(dir), stubResolver{})
	w, err := h.Filewrite(writeReq("/docs/file.txt", pflagWrite|pflagCreat|pflagAppend))
	require.NoError(t, err)

	_, err = w.WriteAt([]byte("BBB\n"), 4) // correct offset from sshfs
	require.NoError(t, err)
	mustCloseWriter(t, w)

	got, err := os.ReadFile(fpath)
	require.NoError(t, err)
	assert.Equal(t, "AAA\nBBB\n", string(got))
}

// TestACLHandlers_Filewrite_AppendOffsetZeroClient documents SSH_FXF_APPEND
// semantics: the server must ignore the client-supplied offset on append handles
// and always write at EOF. WRITE|APPEND with WriteAt("BBB\n", 0) must still
// land at EOF, producing "AAA\nBBB\n". (pkg/sftp's own client sends offset 0.)
func TestACLHandlers_Filewrite_AppendOffsetZeroClient(t *testing.T) {
	dir := t.TempDir()
	fpath := filepath.Join(dir, "file.txt")
	require.NoError(t, os.WriteFile(fpath, []byte("AAA\n"), 0o644))

	h := mkACLHandlers(t, "dev-bob", rwShare(dir), stubResolver{})
	w, err := h.Filewrite(writeReq("/docs/file.txt", pflagWrite|pflagAppend))
	require.NoError(t, err)

	_, err = w.WriteAt([]byte("BBB\n"), 0) // offset 0 from pkg/sftp client
	require.NoError(t, err)
	mustCloseWriter(t, w)

	got, err := os.ReadFile(fpath)
	require.NoError(t, err)
	assert.Equal(t, "AAA\nBBB\n", string(got))
}

// TestACLHandlers_Filewrite_AppendPlusTrunc covers the legal
// truncate-then-append combination: TRUNC empties the file on open, and the
// append writer then lands bytes at the (new) EOF.
func TestACLHandlers_Filewrite_AppendPlusTrunc(t *testing.T) {
	dir := t.TempDir()
	fpath := filepath.Join(dir, "file.txt")
	require.NoError(t, os.WriteFile(fpath, []byte("OLDCONTENT"), 0o644))

	h := mkACLHandlers(t, "dev-bob", rwShare(dir), stubResolver{})
	w, err := h.Filewrite(writeReq("/docs/file.txt", pflagWrite|pflagCreat|pflagAppend|pflagTrunc))
	require.NoError(t, err)

	_, err = w.WriteAt([]byte("BBB\n"), 0)
	require.NoError(t, err)
	mustCloseWriter(t, w)

	got, err := os.ReadFile(fpath)
	require.NoError(t, err)
	assert.Equal(t, "BBB\n", string(got))
}

// TestACLHandlers_Filewrite_UploadSemanticsPreserved verifies that a
// WRITE|CREAT|TRUNC open replaces the existing file content exactly.
func TestACLHandlers_Filewrite_UploadSemanticsPreserved(t *testing.T) {
	dir := t.TempDir()
	fpath := filepath.Join(dir, "file.txt")
	require.NoError(t, os.WriteFile(fpath, []byte("OLDCONTENT"), 0o644))

	h := mkACLHandlers(t, "dev-bob", rwShare(dir), stubResolver{})
	w, err := h.Filewrite(writeReq("/docs/file.txt", pflagWrite|pflagCreat|pflagTrunc))
	require.NoError(t, err)

	_, err = w.WriteAt([]byte("new"), 0)
	require.NoError(t, err)
	mustCloseWriter(t, w)

	got, err := os.ReadFile(fpath)
	require.NoError(t, err)
	assert.Equal(t, "new", string(got))
}

// TestACLHandlers_Filewrite_WriteWithoutCreatOnMissingFile verifies that
// WRITE without CREAT on a nonexistent path returns fs.ErrNotExist.
func TestACLHandlers_Filewrite_WriteWithoutCreatOnMissingFile(t *testing.T) {
	dir := t.TempDir()
	h := mkACLHandlers(t, "dev-bob", rwShare(dir), stubResolver{})

	_, err := h.Filewrite(writeReq("/docs/nonexistent.txt", pflagWrite))
	require.Error(t, err)
	assert.True(t, errors.Is(err, fs.ErrNotExist), "expected fs.ErrNotExist, got %v", err)
}

// TestACLHandlers_Filewrite_ExclHonored verifies that WRITE|CREAT|EXCL on an
// existing file returns fs.ErrExist.
func TestACLHandlers_Filewrite_ExclHonored(t *testing.T) {
	dir := t.TempDir()
	fpath := filepath.Join(dir, "file.txt")
	require.NoError(t, os.WriteFile(fpath, []byte("existing"), 0o644))

	h := mkACLHandlers(t, "dev-bob", rwShare(dir), stubResolver{})
	_, err := h.Filewrite(writeReq("/docs/file.txt", pflagWrite|pflagCreat|pflagExcl))
	require.Error(t, err)
	assert.True(t, errors.Is(err, fs.ErrExist), "expected fs.ErrExist, got %v", err)
}

// TestACLHandlers_Filewrite_DegenerateEmptyFlags verifies that Flags=0 (bare
// Put semantics — no Read or Write bit set) falls back to create+truncate,
// creating the file if absent and replacing any prior content.
func TestACLHandlers_Filewrite_DegenerateEmptyFlags(t *testing.T) {
	dir := t.TempDir()
	fpath := filepath.Join(dir, "new.txt")

	h := mkACLHandlers(t, "dev-bob", rwShare(dir), stubResolver{})
	w, err := h.Filewrite(writeReq("/docs/new.txt", 0))
	require.NoError(t, err)

	_, err = w.WriteAt([]byte("hello"), 0)
	require.NoError(t, err)
	mustCloseWriter(t, w)

	got, err := os.ReadFile(fpath)
	require.NoError(t, err)
	assert.Equal(t, "hello", string(got))
}

// Compile-time assertion: aclHandlers must implement sftp.OpenFileWriter.
// This fails to compile until OpenFile is added — TDD red at the type level.
var _ sftp.OpenFileWriter = (*aclHandlers)(nil)

// TestACLHandlers_OpenFile_RDWRReadModifyWrite verifies the core FUSE-T/NFS
// read-modify-write pattern: open RDWR, ReadAt the existing content, WriteAt
// a merged block at offset 0. Without OpenFile the READ on a write-only handle
// fails and the NFS client merges against a zero page (issue #45).
func TestACLHandlers_OpenFile_RDWRReadModifyWrite(t *testing.T) {
	dir := t.TempDir()
	fpath := filepath.Join(dir, "file.txt")
	require.NoError(t, os.WriteFile(fpath, []byte("AAA\n"), 0o644))

	h := mkACLHandlers(t, "dev-bob", rwShare(dir), stubResolver{})
	rw, err := h.OpenFile(writeReq("/docs/file.txt", pflagRead|pflagWrite))
	require.NoError(t, err)
	require.NotNil(t, rw)

	// Read the existing block first (read-modify-write pattern).
	buf := make([]byte, 4)
	n, err := rw.ReadAt(buf, 0)
	require.NoError(t, err)
	assert.Equal(t, 4, n)
	assert.Equal(t, "AAA\n", string(buf))

	// Write the merged block back — full overwrite at offset 0.
	_, err = rw.WriteAt([]byte("AAA\nBBB\n"), 0)
	require.NoError(t, err)

	c, ok := rw.(io.Closer)
	require.True(t, ok, "OpenFile handle must implement io.Closer")
	require.NoError(t, c.Close())

	got, err := os.ReadFile(fpath)
	require.NoError(t, err)
	assert.Equal(t, "AAA\nBBB\n", string(got))
}

// TestACLHandlers_OpenFile_ReadOnlyShareDenied verifies that OpenFile on a
// read-only share returns sftp.ErrSSHFxPermissionDenied.
func TestACLHandlers_OpenFile_ReadOnlyShareDenied(t *testing.T) {
	dir := t.TempDir()
	acls := []ShareACL{{Alias: "docs", Path: dir, AllowAll: true, ReadOnly: true}}
	h := mkACLHandlers(t, "dev-bob", acls, stubResolver{})

	_, err := h.OpenFile(writeReq("/docs/file.txt", pflagRead|pflagWrite))
	assert.ErrorIs(t, err, sftp.ErrSSHFxPermissionDenied)
}

// TestACLHandlers_OpenFile_RDWRAppend verifies that RDWR+APPEND open supports
// both ReadAt (legal on O_APPEND fds) and WriteAt (which lands at EOF).
func TestACLHandlers_OpenFile_RDWRAppend(t *testing.T) {
	dir := t.TempDir()
	fpath := filepath.Join(dir, "file.txt")
	require.NoError(t, os.WriteFile(fpath, []byte("AAA\n"), 0o644))

	h := mkACLHandlers(t, "dev-bob", rwShare(dir), stubResolver{})
	rw, err := h.OpenFile(writeReq("/docs/file.txt", pflagRead|pflagWrite|pflagAppend))
	require.NoError(t, err)
	require.NotNil(t, rw)

	// ReadAt must work on O_APPEND fds (pread is not restricted).
	buf := make([]byte, 4)
	n, err := rw.ReadAt(buf, 0)
	require.NoError(t, err)
	assert.Equal(t, 4, n)
	assert.Equal(t, "AAA\n", string(buf))

	// WriteAt must land at EOF regardless of supplied offset (APPEND semantics).
	_, err = rw.WriteAt([]byte("BBB\n"), 0)
	require.NoError(t, err)

	c, ok := rw.(io.Closer)
	require.True(t, ok, "OpenFile append handle must implement io.Closer")
	require.NoError(t, c.Close())

	got, err := os.ReadFile(fpath)
	require.NoError(t, err)
	assert.Equal(t, "AAA\nBBB\n", string(got))
}

// TestACLHandlers_OpenFile_NonexistentWithoutCreat verifies that OpenFile of
// a nonexistent path without the CREAT flag returns an fs.ErrNotExist error.
func TestACLHandlers_OpenFile_NonexistentWithoutCreat(t *testing.T) {
	dir := t.TempDir()
	h := mkACLHandlers(t, "dev-bob", rwShare(dir), stubResolver{})

	_, err := h.OpenFile(writeReq("/docs/missing.txt", pflagRead|pflagWrite))
	require.Error(t, err)
	assert.True(t, errors.Is(err, fs.ErrNotExist), "expected fs.ErrNotExist, got %v", err)
}
