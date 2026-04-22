# Enforce per-share ACL (#31) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make `permissions="ro"` and `allowed_devices` in agent `config.kdl` actually gate SFTP access, closing the critical ACL-bypass described in issue #31.

**Architecture:** Replace `sftp.NewServer` with `sftp.NewRequestServer` backed by a custom `sftp.Handlers` that knows the connecting peer's `device_id` (carried through SSH `Permissions.Extensions`) and consults an atomically-swappable ACL snapshot. Remove the global `sftpRoot` symlink directory — the handler translates virtual paths itself. Add a `DeviceResolver` so handlers can match nicknames in `allowed_devices` against the connecting device.

**Tech Stack:** Go 1.25, `github.com/pkg/sftp` v1.13.10 (already in `go.mod`), `golang.org/x/crypto/ssh`, existing KDL config layer, existing scenario-test harness in `tests/scenarios/`.

**Worktree:** `/Users/ykhdr/projects/hubfuse-issue-31` (branch `fix/issue-31-enforce-share-acl`). All paths below are relative to this directory.

---

## File map

**New files:**
- `internal/agent/sharesacl.go` — `ShareACL` type, pure `Decide()` matcher, device-resolver interface.
- `internal/agent/sharesacl_test.go` — unit tests for `Decide()` + path safety.
- `internal/agent/sftphandler.go` — per-connection `sftp.Handlers` implementation.
- `internal/agent/sftphandler_test.go` — unit tests for handler behaviour via `sftp.InMem*`-style request building.
- `tests/scenarios/permissions_test.go` — end-to-end scenario tests (4 cases).

**Modified files:**
- `internal/agent/sshserver.go` — new ACL snapshot pointer, new `UpdateShares` signature, new PublicKeyCallback that stamps `device_id` into `Permissions.Extensions`, rewritten `serveSFTP`, removal of `sftpRoot` machinery.
- `internal/agent/confighandler.go` — replace `sharesToMap` with `sharesToACL`; feed `[]ShareACL` into `sshServer.UpdateShares`; also drive the initial ACL push on startup.
- `internal/agent/daemon.go` — pass a `DeviceResolver` (Daemon itself implements it) to `NewSSHServer`; push initial ACL at startup (the config watcher only fires on file-change events, not on initial state).
- `internal/agent/sshserver_test.go` — update tests for new `UpdateShares` signature; delete symlink-tree tests.
- `internal/agent/daemon_test.go` — update `sharesToMap` tests to `sharesToACL`.
- `tests/scenarios/helpers/agent.go` — extend `WithExport` (or add `WithExportACL`) so tests can set permissions + allowed-devices.
- `README.md` — short note about the secure-default ACL semantics.

---

### Task 1: `ShareACL` value type and pure matcher

**Files:**
- Create: `internal/agent/sharesacl.go`
- Test: `internal/agent/sharesacl_test.go`

- [ ] **Step 1: Write the failing tests**

```go
// internal/agent/sharesacl_test.go
package agent

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

type stubResolver map[string]string // deviceID -> nickname

func (s stubResolver) NicknameForDeviceID(id string) (string, bool) {
	n, ok := s[id]
	return n, ok
}

func TestShareACL_Decide_AllowAllWildcard(t *testing.T) {
	acl := ShareACL{Alias: "foo", Path: "/tmp/foo", ReadOnly: true, AllowAll: true}
	d := acl.Decide("dev-1", stubResolver{"dev-1": "bob"})
	assert.True(t, d.Allow)
}

func TestShareACL_Decide_EmptyDenies(t *testing.T) {
	acl := ShareACL{Alias: "foo", Path: "/tmp/foo"}
	d := acl.Decide("dev-1", stubResolver{"dev-1": "bob"})
	assert.False(t, d.Allow, "empty allowed list must deny")
}

func TestShareACL_Decide_MatchesNickname(t *testing.T) {
	acl := ShareACL{Alias: "foo", Path: "/tmp/foo", AllowedDevices: []string{"bob"}}
	d := acl.Decide("dev-1", stubResolver{"dev-1": "bob"})
	assert.True(t, d.Allow)
}

func TestShareACL_Decide_MatchesDeviceID(t *testing.T) {
	acl := ShareACL{Alias: "foo", Path: "/tmp/foo", AllowedDevices: []string{"dev-1"}}
	d := acl.Decide("dev-1", stubResolver{})
	assert.True(t, d.Allow, "device_id match should work even without nickname resolver entry")
}

func TestShareACL_Decide_NoMatchDenies(t *testing.T) {
	acl := ShareACL{Alias: "foo", Path: "/tmp/foo", AllowedDevices: []string{"carol"}}
	d := acl.Decide("dev-1", stubResolver{"dev-1": "bob"})
	assert.False(t, d.Allow)
}

func TestShareACL_Decide_ReadOnlyFlag(t *testing.T) {
	acl := ShareACL{Alias: "foo", Path: "/tmp/foo", ReadOnly: true, AllowAll: true}
	d := acl.Decide("dev-1", stubResolver{})
	assert.True(t, d.Allow)
	assert.True(t, d.ReadOnly)
}

func TestFromShareConfigs_Defaults(t *testing.T) {
	// Empty permissions → ro; literal "all" → AllowAll; explicit list preserved.
	in := []shareConfigView{
		{Alias: "a", Path: "/p/a", Permissions: "", AllowedDevices: nil},
		{Alias: "b", Path: "/p/b", Permissions: "rw", AllowedDevices: []string{"all"}},
		{Alias: "c", Path: "/p/c", Permissions: "ro", AllowedDevices: []string{"bob", "carol"}},
	}
	got := ShareACLsFromConfig(in)
	assert.Len(t, got, 3)

	assert.Equal(t, "a", got[0].Alias)
	assert.True(t, got[0].ReadOnly, "missing permissions defaults to ro")
	assert.False(t, got[0].AllowAll)
	assert.Empty(t, got[0].AllowedDevices)

	assert.Equal(t, "b", got[1].Alias)
	assert.False(t, got[1].ReadOnly)
	assert.True(t, got[1].AllowAll, `"all" must be recognised as wildcard`)

	assert.Equal(t, "c", got[2].Alias)
	assert.True(t, got[2].ReadOnly)
	assert.False(t, got[2].AllowAll)
	assert.Equal(t, []string{"bob", "carol"}, got[2].AllowedDevices)
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd /Users/ykhdr/projects/hubfuse-issue-31 && go test ./internal/agent/ -run 'ShareACL|FromShareConfigs' -v`
Expected: FAIL — `ShareACL`, `ShareACLsFromConfig`, and `shareConfigView` not defined.

- [ ] **Step 3: Implement the matcher**

```go
// internal/agent/sharesacl.go
package agent

// DeviceResolver maps a peer device_id to its hub-advertised nickname.
// Returns ok=false if the mapping is not yet known (e.g. right after pairing,
// before the hub has emitted a DeviceOnline event).
type DeviceResolver interface {
	NicknameForDeviceID(id string) (string, bool)
}

// ShareACL is a runtime, flattened representation of a share's access policy.
// It is derived from config.ShareConfig by ShareACLsFromConfig.
type ShareACL struct {
	Alias          string
	Path           string
	ReadOnly       bool
	AllowAll       bool     // true when AllowedDevices contained the literal "all"
	AllowedDevices []string // remaining tokens (nicknames or device_ids)
}

// AccessDecision is the result of evaluating a ShareACL for a specific peer.
type AccessDecision struct {
	Allow    bool
	ReadOnly bool
}

// Decide returns whether deviceID may access the share and, if so, whether
// the share is read-only.
func (a ShareACL) Decide(deviceID string, resolver DeviceResolver) AccessDecision {
	if a.AllowAll {
		return AccessDecision{Allow: true, ReadOnly: a.ReadOnly}
	}
	if len(a.AllowedDevices) == 0 {
		return AccessDecision{Allow: false}
	}
	var nickname string
	if resolver != nil {
		if n, ok := resolver.NicknameForDeviceID(deviceID); ok {
			nickname = n
		}
	}
	for _, tok := range a.AllowedDevices {
		if tok == deviceID || (nickname != "" && tok == nickname) {
			return AccessDecision{Allow: true, ReadOnly: a.ReadOnly}
		}
	}
	return AccessDecision{Allow: false}
}

// shareConfigView is the minimal shape of config.ShareConfig that the ACL
// layer depends on. Defined here to keep sharesacl.go free of a direct
// dependency on internal/agent/config at test time.
type shareConfigView struct {
	Alias          string
	Path           string
	Permissions    string
	AllowedDevices []string
}

// ShareACLsFromConfig flattens a slice of share configs into runtime ACLs.
// Applies secure defaults: missing permissions = "ro"; the reserved token
// "all" is lifted into AllowAll and removed from the token list.
func ShareACLsFromConfig(shares []shareConfigView) []ShareACL {
	out := make([]ShareACL, 0, len(shares))
	for _, s := range shares {
		acl := ShareACL{
			Alias:    s.Alias,
			Path:     s.Path,
			ReadOnly: s.Permissions != "rw", // "" and anything other than "rw" → ro
		}
		for _, tok := range s.AllowedDevices {
			if tok == "all" {
				acl.AllowAll = true
				continue
			}
			acl.AllowedDevices = append(acl.AllowedDevices, tok)
		}
		out = append(out, acl)
	}
	return out
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/agent/ -run 'ShareACL|FromShareConfigs' -v`
Expected: PASS (all 7 subtests).

- [ ] **Step 5: Commit**

```bash
git add internal/agent/sharesacl.go internal/agent/sharesacl_test.go
git commit -m "feat(agent): ShareACL value + pure matcher

Secure defaults: missing permissions treated as ro; empty allowed list
denies; literal \"all\" is a wildcard. Pure function, no I/O — easy to
unit-test and drop into an SFTP handler later."
```

---

### Task 2: Path-safety helper (resolve virtual path to real path under share root)

**Files:**
- Modify: `internal/agent/sharesacl.go`
- Test: `internal/agent/sharesacl_test.go`

- [ ] **Step 1: Write the failing tests**

Append to `internal/agent/sharesacl_test.go`:

```go
func TestResolveSharePath_Basic(t *testing.T) {
	got, err := ResolveSharePath("/srv/docs", "/docs/a/b.txt", "docs")
	assert.NoError(t, err)
	assert.Equal(t, "/srv/docs/a/b.txt", got)
}

func TestResolveSharePath_AliasRoot(t *testing.T) {
	got, err := ResolveSharePath("/srv/docs", "/docs", "docs")
	assert.NoError(t, err)
	assert.Equal(t, "/srv/docs", got)
}

func TestResolveSharePath_TrailingSlash(t *testing.T) {
	got, err := ResolveSharePath("/srv/docs", "/docs/", "docs")
	assert.NoError(t, err)
	assert.Equal(t, "/srv/docs", got)
}

func TestResolveSharePath_RejectsTraversal(t *testing.T) {
	_, err := ResolveSharePath("/srv/docs", "/docs/../../etc/passwd", "docs")
	assert.Error(t, err, "must reject path that escapes share root after cleaning")
}

func TestResolveSharePath_RejectsWrongAlias(t *testing.T) {
	_, err := ResolveSharePath("/srv/docs", "/other/file", "docs")
	assert.Error(t, err, "alias mismatch must error")
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/agent/ -run 'ResolveSharePath' -v`
Expected: FAIL — `ResolveSharePath` not defined.

- [ ] **Step 3: Implement resolver**

Append to `internal/agent/sharesacl.go`:

```go
import (
	"fmt"
	"path"
	"path/filepath"
	"strings"
)

// ResolveSharePath translates a virtual SFTP path of the form
// "/<alias>/sub/path" into a cleaned real filesystem path under shareRoot.
// Returns an error if the first segment does not match expectedAlias, or if
// the result would escape shareRoot after cleaning.
func ResolveSharePath(shareRoot, virtualPath, expectedAlias string) (string, error) {
	// path.Clean on posix-shaped SFTP paths, not filepath.Clean.
	cleaned := path.Clean("/" + strings.TrimPrefix(virtualPath, "/"))

	// Strip leading slash then split off the alias.
	trimmed := strings.TrimPrefix(cleaned, "/")
	var alias, rest string
	if i := strings.Index(trimmed, "/"); i >= 0 {
		alias = trimmed[:i]
		rest = trimmed[i+1:]
	} else {
		alias = trimmed
	}
	if alias != expectedAlias {
		return "", fmt.Errorf("path %q does not belong to share %q", virtualPath, expectedAlias)
	}

	// Join with the real root using filepath (OS-specific separators) and
	// verify the result is still under the root after cleaning.
	root := filepath.Clean(shareRoot)
	joined := filepath.Clean(filepath.Join(root, rest))
	if joined != root && !strings.HasPrefix(joined, root+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q escapes share root", virtualPath)
	}
	return joined, nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/agent/ -run 'ResolveSharePath' -v`
Expected: PASS (5 subtests).

- [ ] **Step 5: Commit**

```bash
git add internal/agent/sharesacl.go internal/agent/sharesacl_test.go
git commit -m "feat(agent): ResolveSharePath helper with traversal rejection

Cleans the posix-shaped SFTP path, verifies alias prefix, and confirms
the translated filesystem path stays under the share root."
```

---

### Task 3: Stamp `device_id` into `*ssh.Permissions` during auth + reverse-index keys

**Files:**
- Modify: `internal/agent/sshserver.go`
- Modify: `internal/agent/reload_keys_test.go` (callsite update)
- Modify: `internal/agent/daemon_test.go` (callsite update if needed)

- [ ] **Step 1: Write the failing test**

Create a new test in `internal/agent/sshserver_test.go` (insert at the end of the file):

```go
func TestSSHServer_PublicKeyCallback_StampsDeviceID(t *testing.T) {
	tmp := t.TempDir()
	keyPath := generateTestHostKey(t, tmp) // existing helper in this test file
	srv, err := NewSSHServer(0, keyPath, discardLogger())
	require.NoError(t, err)

	pub, _ := generateTestKeyPair(t)
	srv.UpdateAllowedKeys(map[string]gossh.PublicKey{"dev-bob": pub})

	perms, err := srv.publicKeyCallback(nil, pub)
	require.NoError(t, err)
	require.NotNil(t, perms)
	assert.Equal(t, "dev-bob", perms.Extensions["hubfuse-device-id"],
		"device_id from UpdateAllowedKeys must be propagated via ssh.Permissions.Extensions")
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/agent/ -run 'TestSSHServer_PublicKeyCallback_StampsDeviceID' -v`
Expected: FAIL — current `publicKeyCallback` returns `&gossh.Permissions{}` with no Extensions.

- [ ] **Step 3: Rewire `allowedKeys` / `publicKeyCallback`**

In `internal/agent/sshserver.go`, replace the `allowedKeys`/`allowedKeyCache` fields and their writers with a single reverse-index from key fingerprint to `device_id`:

```go
// Replace these two fields:
//   allowedKeys     map[string]gossh.PublicKey
//   allowedKeyCache map[string]bool
// with:
deviceIDByFingerprint map[string]string // key.Marshal() -> device_id
```

Rewrite `UpdateAllowedKeys`:

```go
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
```

Rewrite `publicKeyCallback`:

```go
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
```

Update `NewSSHServer` to initialise `deviceIDByFingerprint: make(map[string]string)` and drop initialisation of the removed fields.

- [ ] **Step 4: Update existing tests that poke at the old fields**

In `internal/agent/sshserver_test.go`, any test asserting on `srv.allowedKeys` or `srv.allowedKeyCache` must now assert on `srv.deviceIDByFingerprint`. Replace:

```go
// before: srv.allowedKeyCache[string(pub.Marshal())]
// after:
id, ok := srv.deviceIDByFingerprint[string(pub.Marshal())]
assert.True(t, ok)
assert.Equal(t, "dev-bob", id)
```

In `internal/agent/reload_keys_test.go`, callers of `publicKeyCallback` now receive `Permissions.Extensions["hubfuse-device-id"]`; keep existing allow/deny assertions, they still hold (err == nil / err != nil).

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/agent/ -run 'SSHServer|reload' -v`
Expected: PASS — `TestSSHServer_PublicKeyCallback_StampsDeviceID` + all existing key/auth tests green.

- [ ] **Step 6: Commit**

```bash
git add internal/agent/sshserver.go internal/agent/sshserver_test.go internal/agent/reload_keys_test.go
git commit -m "feat(agent): propagate device_id via ssh.Permissions.Extensions

Replace the fingerprint->bool cache with a fingerprint->device_id index
so the SFTP layer can identify the peer. PublicKeyCallback stamps the
device_id into ssh.Permissions.Extensions under key \"hubfuse-device-id\"."
```

---

### Task 4: `sshServer` stores an atomic `[]ShareACL` snapshot (new `UpdateShares`)

**Files:**
- Modify: `internal/agent/sshserver.go`

- [ ] **Step 1: Write the failing test**

Append to `internal/agent/sshserver_test.go`:

```go
func TestSSHServer_UpdateShares_StoresACLSnapshot(t *testing.T) {
	tmp := t.TempDir()
	keyPath := generateTestHostKey(t, tmp)
	srv, err := NewSSHServer(0, keyPath, discardLogger())
	require.NoError(t, err)

	acls := []ShareACL{{Alias: "docs", Path: "/tmp/docs", ReadOnly: true, AllowAll: true}}
	srv.UpdateShares(acls)

	snap := srv.aclSnapshot()
	require.Len(t, snap, 1)
	assert.Equal(t, "docs", snap[0].Alias)
	assert.True(t, snap[0].ReadOnly)
	assert.True(t, snap[0].AllowAll)

	// Second update replaces the snapshot atomically.
	srv.UpdateShares([]ShareACL{{Alias: "photos", Path: "/tmp/photos", AllowAll: true}})
	snap = srv.aclSnapshot()
	require.Len(t, snap, 1)
	assert.Equal(t, "photos", snap[0].Alias)
}
```

- [ ] **Step 2: Run the test**

Run: `go test ./internal/agent/ -run 'TestSSHServer_UpdateShares_StoresACLSnapshot' -v`
Expected: FAIL — `UpdateShares([]ShareACL)` signature and `aclSnapshot()` accessor do not exist.

- [ ] **Step 3: Implement**

In `internal/agent/sshserver.go`:

1. Add `"sync/atomic"` to imports.
2. Drop the `shares map[string]string` and `sftpRoot` fields. Drop the `sftpRoot` argument handling in `NewSSHServer`.
3. Add field:
   ```go
   acls atomic.Pointer[[]ShareACL]
   ```
4. In `NewSSHServer`, initialise with an empty snapshot:
   ```go
   empty := []ShareACL{}
   s.acls.Store(&empty)
   ```
5. Replace `UpdateShares`:
   ```go
   func (s *SSHServer) UpdateShares(shares []ShareACL) {
       cp := append([]ShareACL(nil), shares...)
       s.acls.Store(&cp)
   }
   ```
6. Add an accessor used by handlers and tests:
   ```go
   func (s *SSHServer) aclSnapshot() []ShareACL {
       p := s.acls.Load()
       if p == nil {
           return nil
       }
       return *p
   }
   ```
7. Delete `rebuildSFTPRoot`, the symlink cleanup code, and the `sftpRoot` derivation in `NewSSHServer`.

- [ ] **Step 4: Run tests**

Run: `go test ./internal/agent/ -run 'TestSSHServer_UpdateShares_StoresACLSnapshot' -v`
Expected: PASS.

The old tests `TestSSHServer_UpdateShares_RebuildsSFTPRoot` / `..._ReplacesOldSymlinks` / `TestSSHServer_SFTPFileAccess` will now fail to compile (wrong arg type) or fail at runtime — that's fine; they are superseded and will be deleted in Task 6.

- [ ] **Step 5: Commit**

```bash
git add internal/agent/sshserver.go internal/agent/sshserver_test.go
git commit -m "feat(agent): atomic []ShareACL snapshot on sshServer

Replaces map[alias]path + symlink tree. Readers dereference
atomic.Pointer[[]ShareACL] without locking; writers (config watcher)
swap in a fresh slice copy. Prepares for the custom sftp.Handlers."
```

---

### Task 5: Custom `sftp.Handlers` enforcing the ACL

**Files:**
- Create: `internal/agent/sftphandler.go`
- Create: `internal/agent/sftphandler_test.go`

- [ ] **Step 1: Write the failing tests**

```go
// internal/agent/sftphandler_test.go
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
	h := newACLHandlers(deviceID, r, func() []ShareACL { return acls }, discardLogger())
	return h
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
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/agent/ -run 'ACLHandlers' -v`
Expected: FAIL — `newACLHandlers`, `aclHandlers`, handler methods not defined.

- [ ] **Step 3: Implement the handler**

```go
// internal/agent/sftphandler.go
package agent

import (
	"io"
	"log/slog"
	"os"
	"path"
	"strings"
	"time"

	"github.com/pkg/sftp"
)

// aclHandlers is a per-connection sftp.Handlers implementation. It captures
// the connecting peer's device_id and reads a fresh []ShareACL snapshot on
// every request so fsnotify-driven config reloads take effect immediately.
type aclHandlers struct {
	deviceID string
	resolver DeviceResolver
	snapshot func() []ShareACL
	logger   *slog.Logger
}

func newACLHandlers(deviceID string, r DeviceResolver, snap func() []ShareACL, logger *slog.Logger) *aclHandlers {
	return &aclHandlers{deviceID: deviceID, resolver: r, snapshot: snap, logger: logger}
}

// ToHandlers produces the sftp.Handlers value the request server needs.
func (h *aclHandlers) ToHandlers() sftp.Handlers {
	return sftp.Handlers{
		FileGet:  h,
		FilePut:  h,
		FileCmd:  h,
		FileList: h,
	}
}

func (h *aclHandlers) findShare(alias string) (ShareACL, AccessDecision, bool) {
	for _, a := range h.snapshot() {
		if a.Alias == alias {
			return a, a.Decide(h.deviceID, h.resolver), true
		}
	}
	return ShareACL{}, AccessDecision{}, false
}

// splitVirtual splits the virtual SFTP path into the alias and the remainder.
// The path is cleaned; returns ok=false for the synthetic root "/".
func splitVirtual(virtual string) (alias, rest string, ok bool) {
	cleaned := path.Clean("/" + strings.TrimPrefix(virtual, "/"))
	if cleaned == "/" {
		return "", "", false
	}
	trimmed := strings.TrimPrefix(cleaned, "/")
	if i := strings.Index(trimmed, "/"); i >= 0 {
		return trimmed[:i], trimmed[i+1:], true
	}
	return trimmed, "", true
}

func denied() error { return sftp.ErrSSHFxPermissionDenied }

// Fileread — implements sftp.FileReader.
func (h *aclHandlers) Fileread(r *sftp.Request) (io.ReaderAt, error) {
	alias, _, ok := splitVirtual(r.Filepath)
	if !ok {
		return nil, denied()
	}
	acl, dec, found := h.findShare(alias)
	if !found || !dec.Allow {
		return nil, denied()
	}
	real, err := ResolveSharePath(acl.Path, r.Filepath, alias)
	if err != nil {
		return nil, denied()
	}
	f, err := os.Open(real)
	if err != nil {
		return nil, err
	}
	return f, nil
}

// Filewrite — implements sftp.FileWriter.
func (h *aclHandlers) Filewrite(r *sftp.Request) (io.WriterAt, error) {
	alias, _, ok := splitVirtual(r.Filepath)
	if !ok {
		return nil, denied()
	}
	acl, dec, found := h.findShare(alias)
	if !found || !dec.Allow || dec.ReadOnly {
		return nil, denied()
	}
	real, err := ResolveSharePath(acl.Path, r.Filepath, alias)
	if err != nil {
		return nil, denied()
	}
	f, err := os.OpenFile(real, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return nil, err
	}
	return f, nil
}

// Filecmd — implements sftp.FileCmder. All methods routed here are write-class.
func (h *aclHandlers) Filecmd(r *sftp.Request) error {
	alias, _, ok := splitVirtual(r.Filepath)
	if !ok {
		return denied()
	}
	acl, dec, found := h.findShare(alias)
	if !found || !dec.Allow || dec.ReadOnly {
		return denied()
	}
	real, err := ResolveSharePath(acl.Path, r.Filepath, alias)
	if err != nil {
		return denied()
	}
	switch r.Method {
	case "Setstat":
		if r.Attributes() != nil && r.Attributes().FileMode() != 0 {
			if err := os.Chmod(real, r.Attributes().FileMode()); err != nil {
				return err
			}
		}
		return nil
	case "Rename":
		targetAlias, _, tok := splitVirtual(r.Target)
		if !tok || targetAlias != alias {
			return denied()
		}
		targetReal, err := ResolveSharePath(acl.Path, r.Target, alias)
		if err != nil {
			return denied()
		}
		return os.Rename(real, targetReal)
	case "Rmdir":
		return os.Remove(real)
	case "Remove":
		return os.Remove(real)
	case "Mkdir":
		return os.Mkdir(real, 0o755)
	case "Link":
		targetAlias, _, tok := splitVirtual(r.Target)
		if !tok || targetAlias != alias {
			return denied()
		}
		targetReal, err := ResolveSharePath(acl.Path, r.Target, alias)
		if err != nil {
			return denied()
		}
		return os.Link(real, targetReal)
	case "Symlink":
		return os.Symlink(r.Target, real)
	}
	return denied()
}

// Filelist — implements sftp.FileLister.
func (h *aclHandlers) Filelist(r *sftp.Request) (sftp.ListerAt, error) {
	// Synthetic root: list only allowed shares.
	if r.Filepath == "/" || r.Filepath == "" {
		var infos []os.FileInfo
		for _, a := range h.snapshot() {
			if !a.Decide(h.deviceID, h.resolver).Allow {
				continue
			}
			fi, err := os.Stat(a.Path)
			if err != nil {
				continue // share directory missing on disk → skip
			}
			infos = append(infos, renamedFileInfo{FileInfo: fi, name: a.Alias})
		}
		return listerAt(infos), nil
	}

	alias, _, ok := splitVirtual(r.Filepath)
	if !ok {
		return nil, denied()
	}
	acl, dec, found := h.findShare(alias)
	if !found || !dec.Allow {
		return nil, denied()
	}
	real, err := ResolveSharePath(acl.Path, r.Filepath, alias)
	if err != nil {
		return nil, denied()
	}

	switch r.Method {
	case "List":
		f, err := os.Open(real)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		entries, err := f.Readdir(-1)
		if err != nil {
			return nil, err
		}
		return listerAt(entries), nil
	case "Stat":
		fi, err := os.Stat(real)
		if err != nil {
			return nil, err
		}
		return listerAt([]os.FileInfo{fi}), nil
	case "Lstat":
		fi, err := os.Lstat(real)
		if err != nil {
			return nil, err
		}
		return listerAt([]os.FileInfo{fi}), nil
	case "Readlink":
		target, err := os.Readlink(real)
		if err != nil {
			return nil, err
		}
		return listerAt([]os.FileInfo{renamedFileInfo{FileInfo: staticLink(target), name: target}}), nil
	}
	return nil, denied()
}

// listerAt is a trivial sftp.ListerAt over a slice.
type listerAt []os.FileInfo

func (l listerAt) ListAt(p []os.FileInfo, off int64) (int, error) {
	if off >= int64(len(l)) {
		return 0, io.EOF
	}
	n := copy(p, l[off:])
	if off+int64(n) >= int64(len(l)) {
		return n, io.EOF
	}
	return n, nil
}

// renamedFileInfo overrides Name() while delegating all other methods.
type renamedFileInfo struct {
	os.FileInfo
	name string
}

func (r renamedFileInfo) Name() string { return r.name }

// staticLink is a minimal os.FileInfo that reports only a name (used for Readlink).
type staticLink string

func (s staticLink) Name() string       { return string(s) }
func (s staticLink) Size() int64        { return int64(len(s)) }
func (s staticLink) Mode() os.FileMode  { return os.ModeSymlink | 0o777 }
func (s staticLink) ModTime() time.Time { return time.Time{} }
func (s staticLink) IsDir() bool        { return false }
func (s staticLink) Sys() any           { return nil }
```

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/agent/ -run 'ACLHandlers' -v`
Expected: PASS (all 6 subtests).

- [ ] **Step 5: Commit**

```bash
git add internal/agent/sftphandler.go internal/agent/sftphandler_test.go
git commit -m "feat(agent): per-connection sftp.Handlers with ACL enforcement

aclHandlers resolves the virtual path to the real share directory,
denies unknown or not-allowed shares, and blocks every write-class
method (Put/Setstat/Rename/Remove/Rmdir/Mkdir/Symlink/Link) on shares
marked read-only. Synthetic root listing returns only shares visible
to the connecting device_id."
```

---

### Task 6: Wire `serveSFTP` to the new handler, delete symlink tree

**Files:**
- Modify: `internal/agent/sshserver.go`
- Modify: `internal/agent/sshserver_test.go` (delete superseded tests)

- [ ] **Step 1: Delete the obsolete test functions**

In `internal/agent/sshserver_test.go`, remove these functions entirely:
- `TestSSHServer_UpdateShares_RebuildsSFTPRoot`
- `TestSSHServer_UpdateShares_ReplacesOldSymlinks`
- `TestSSHServer_SFTPFileAccess`

They test the removed `sftpRoot` contract. Equivalent coverage is in `sftphandler_test.go` (unit) and the upcoming scenario tests (e2e).

- [ ] **Step 2: Rewrite `serveSFTP`**

In `internal/agent/sshserver.go`, replace `serveSFTP` with:

```go
func (s *SSHServer) serveSFTP(channel gossh.Channel, deviceID string) {
	h := newACLHandlers(deviceID, s.resolver, s.aclSnapshot, s.logger)
	srv := sftp.NewRequestServer(channel, h.ToHandlers())
	defer srv.Close()

	if err := srv.Serve(); err != nil {
		s.logger.Debug("sftp session ended", "device_id", deviceID, "error", err)
	}
}
```

Update `handleChannel` to pass the `device_id` that `handleConn` extracted from `ssh.Permissions`. Replace the signature:

```go
func (s *SSHServer) handleChannel(newChan gossh.NewChannel, deviceID string) {
    // ...existing channel accept code...
    // ...when subsystem == "sftp":
    s.serveSFTP(channel, deviceID)
    return
}
```

Update `handleConn` to read the device_id and pass it down:

```go
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
		"remote", sshConn.RemoteAddr(), "user", sshConn.User(), "device_id", deviceID)

	go gossh.DiscardRequests(reqs)
	for newChan := range chans {
		go s.handleChannel(newChan, deviceID)
	}
}
```

Add a `resolver DeviceResolver` field to `SSHServer` + a setter `SetDeviceResolver(r DeviceResolver)` so the daemon can wire itself in after construction (daemon and sshServer have a chicken-and-egg relationship otherwise).

```go
// In the struct:
resolver DeviceResolver

// As a method:
func (s *SSHServer) SetDeviceResolver(r DeviceResolver) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resolver = r
}
```

Update `newACLHandlers` call in `serveSFTP` to read the resolver under the same lock snapshot (or add a `resolverSnapshot()` accessor analogous to `aclSnapshot`). Simplest: read once at call time:

```go
func (s *SSHServer) currentResolver() DeviceResolver {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.resolver
}
```

and use it in `serveSFTP`:

```go
h := newACLHandlers(deviceID, s.currentResolver(), s.aclSnapshot, s.logger)
```

Remove all references to `s.sftpRoot` (field declaration, initialisation, `MkdirAll` calls) and the old `os`/`path/filepath` imports that are no longer needed.

- [ ] **Step 3: Ensure the package still compiles**

Run: `go build ./internal/agent/...`
Expected: PASS.

- [ ] **Step 4: Run the SSH/handler tests**

Run: `go test ./internal/agent/ -run 'SSHServer|ACLHandlers|ResolveSharePath|ShareACL|FromShareConfigs|reload' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/agent/sshserver.go internal/agent/sshserver_test.go
git commit -m "refactor(agent): route SFTP through aclHandlers, drop sftpRoot

serveSFTP now wires a per-connection aclHandlers that carries the
authenticated device_id and reads the live ACL snapshot. The symlink
tree under sftp-root is gone; virtual paths are translated inside the
handler."
```

---

### Task 7: Wire config → ACL through the daemon (including initial push)

**Files:**
- Modify: `internal/agent/confighandler.go`
- Modify: `internal/agent/daemon.go`
- Modify: `internal/agent/daemon_test.go` (update `sharesToMap` callsites)

- [ ] **Step 1: Rewrite `sharesToMap` as `sharesToACL`**

In `internal/agent/confighandler.go`, replace the function:

```go
// sharesToACL flattens a slice of ShareConfig into runtime ACLs, applying the
// secure defaults that ShareACLsFromConfig describes.
func sharesToACL(shares []agentconfig.ShareConfig) []ShareACL {
	views := make([]shareConfigView, 0, len(shares))
	for _, s := range shares {
		views = append(views, shareConfigView{
			Alias:          s.Alias,
			Path:           s.Path,
			Permissions:    s.Permissions,
			AllowedDevices: s.AllowedDevices,
		})
	}
	return ShareACLsFromConfig(views)
}
```

In the same file, change the `onConfigChange` block that pushed to the SSH server:

```go
// before:  d.sshServer.UpdateShares(sharesToMap(new.Shares))
// after:
d.sshServer.UpdateShares(sharesToACL(new.Shares))
```

Also log a warning for any share that is unreachable under the new semantics:

```go
for _, acl := range sharesToACL(new.Shares) {
	if !acl.AllowAll && len(acl.AllowedDevices) == 0 {
		d.logger.Warn("share has no allowed-devices and is inaccessible", "alias", acl.Alias)
	}
}
```

- [ ] **Step 2: Initial push at daemon startup + DeviceResolver wiring**

The config watcher's `onChange` fires only on file-change events. Without an initial push, shares declared in `config.kdl` on startup would never be registered with the SSH server (this is the reason the scenario helper hacks around it with `share add` after daemon start). Fix it properly.

In `internal/agent/daemon.go`, after `d.sshServer` is created in `NewDaemon` (or at the start of `Run` before `startSSH`):

```go
// Install the initial ACL snapshot so pre-existing shares are enforced
// immediately — the config watcher only fires on later file-change events.
d.sshServer.UpdateShares(sharesToACL(cfg.Shares))

// Daemon satisfies DeviceResolver (method added below). Inject it so the
// sftp handler can match nicknames in allowed_devices.
d.sshServer.SetDeviceResolver(d)
```

Add a method on `Daemon` to satisfy `DeviceResolver`:

```go
// NicknameForDeviceID implements agent.DeviceResolver.
func (d *Daemon) NicknameForDeviceID(id string) (string, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if dev, ok := d.onlineDevices[id]; ok && dev.Nickname != "" {
		return dev.Nickname, true
	}
	return "", false
}
```

- [ ] **Step 3: Update daemon tests that reference `sharesToMap`**

In `internal/agent/daemon_test.go`, every use of `sharesToMap(...)` becomes `sharesToACL(...)`, and the return-type assertions change from `map[alias]path` to `[]ShareACL`. Concretely:

```go
// before:
sharesMap := sharesToMap(newCfg.Shares)
_, ok := sharesMap["new-share"]
assert.True(t, ok, "sharesToMap missing new-share")
d.sshServer.UpdateShares(sharesMap)

// after:
acls := sharesToACL(newCfg.Shares)
found := false
for _, a := range acls {
    if a.Alias == "new-share" {
        found = true
        break
    }
}
assert.True(t, found, "sharesToACL missing new-share")
d.sshServer.UpdateShares(acls)
```

And the post-condition that poked `d.sshServer.shares["new-share"]` becomes an assertion on `aclSnapshot()`:

```go
snap := d.sshServer.aclSnapshot()
hasNew := false
for _, a := range snap {
    if a.Alias == "new-share" { hasNew = true }
}
assert.True(t, hasNew, "sshServer snapshot missing 'new-share' after UpdateShares")
```

- [ ] **Step 4: Build and test**

Run: `go build ./... && go test ./internal/agent/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/agent/confighandler.go internal/agent/daemon.go internal/agent/daemon_test.go
git commit -m "feat(agent): daemon pushes ACL at startup and resolves nicknames

sharesToACL replaces sharesToMap. Daemon implements DeviceResolver via
its onlineDevices map and installs the initial ACL snapshot before the
SSH server starts accepting connections, so shares declared in
config.kdl are enforced from the first connection."
```

---

### Task 8: Warn log covers initial startup too + sanity vet

**Files:**
- Modify: `internal/agent/daemon.go`

- [ ] **Step 1: Ensure startup also runs the "empty allowed-devices" warning**

In `NewDaemon` (right after the first `UpdateShares`), iterate the ACLs and log:

```go
for _, acl := range sharesToACL(cfg.Shares) {
	if !acl.AllowAll && len(acl.AllowedDevices) == 0 {
		logger.Warn("share has no allowed-devices and is inaccessible", "alias", acl.Alias)
	}
}
```

(Or refactor both callsites into a small `warnAboutInaccessibleShares(logger, acls)` helper and call it from both the daemon init and `onConfigChange`. YAGNI-check: if the loop body stays 3 lines, a helper is overkill — inline twice is fine.)

- [ ] **Step 2: Sanity vet and full internal test run**

Run: `go vet ./... && go test ./internal/...`
Expected: PASS.

- [ ] **Step 3: Commit**

```bash
git add internal/agent/daemon.go
git commit -m "feat(agent): warn on startup for shares without allowed-devices"
```

---

### Task 9: Scenario helper: allow tests to set permissions + allowed-devices on exports

**Files:**
- Modify: `tests/scenarios/helpers/agent.go`

- [ ] **Step 1: Extend the `share` struct and CLI invocation**

In `tests/scenarios/helpers/agent.go`, change the private `share` type and the `WithExport`/`StartDaemon` machinery:

```go
type share struct {
	path        string
	alias       string
	permissions string   // "" = use CLI default ("ro"); "rw" or "ro"
	allow       []string // tokens for --allow (nicknames, "all", etc.)
}

// WithExport appends a directory export with the given alias. Defaults:
// permissions="ro" (CLI default), allow=["all"] (explicit wildcard to keep
// backward compatibility with existing scenario tests).
func WithExport(path, alias string) AgentOption {
	return func(a *Agent) {
		a.exports = append(a.exports, share{path: path, alias: alias, allow: []string{"all"}})
	}
}

// WithExportACL appends a directory export with explicit permissions and
// allowed-devices. Use this in tests that exercise ACL behaviour.
func WithExportACL(path, alias, permissions string, allow ...string) AgentOption {
	return func(a *Agent) {
		a.exports = append(a.exports, share{path: path, alias: alias, permissions: permissions, allow: allow})
	}
}
```

Update the `share add` invocation in `StartDaemon`:

```go
for _, s := range a.exports {
	if mkErr := os.MkdirAll(s.path, 0o755); mkErr != nil {
		t.Fatalf("mkdir export %s: %v", s.path, mkErr)
	}
	args := []string{"share", "add", s.path, "--alias", s.alias}
	if s.permissions != "" {
		args = append(args, "--permissions", s.permissions)
	}
	for _, dev := range s.allow {
		args = append(args, "--allow", dev)
	}
	a.run(t, args...)
}
```

- [ ] **Step 2: Run existing scenario tests to confirm no regression**

Run: `go test ./tests/scenarios/... -run TestPairAndMountBasic -timeout 60s -v`
Expected: PASS. The default `WithExport` now sends `--allow all`, which keeps the scenario's expected visibility.

- [ ] **Step 3: Commit**

```bash
git add tests/scenarios/helpers/agent.go
git commit -m "test(scenarios): WithExportACL helper for ACL scenarios

Existing WithExport now defaults to --allow all so pre-ACL scenarios
keep working. New WithExportACL lets scenario tests declare explicit
permissions and allowed-devices."
```

---

### Task 10: Scenario — read-only share rejects writes

**Files:**
- Create: `tests/scenarios/permissions_test.go`

- [ ] **Step 1: Write the scenario test**

```go
// tests/scenarios/permissions_test.go
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

	"github.com/ykhdr/hubfuse/internal/common"
	"github.com/ykhdr/hubfuse/tests/scenarios/helpers"
)

// dialSFTPAs connects directly to peer's SSH port using dialerAgent's SSH
// private key. It returns the sftp client; caller must Close it.
func dialSFTPAs(t *testing.T, dialer *helpers.Agent, peer *helpers.Agent) *sftp.Client {
	t.Helper()
	keyPath := filepath.Join(dialer.HomeDir, ".hubfuse", "keys", "id_ed25519")
	raw, err := os.ReadFile(keyPath)
	require.NoError(t, err, "read dialer ssh key")
	signer, err := gossh.ParsePrivateKey(raw)
	require.NoError(t, err, "parse dialer ssh key")
	cfg := &gossh.ClientConfig{
		User:            common.AgentSSHUser,
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

	alice := helpers.StartAgent(t, hub, "alice", helpers.WithExportACL(exportDir, "docs", "ro", "bob"))
	alice.Join(t)
	alice.StartDaemon(t)

	bob := helpers.StartAgent(t, hub, "bob")
	bob.Join(t)
	bob.StartDaemon(t)

	require.Eventually(t,
		func() bool { return alice.HasPeer(t, "bob") && bob.HasPeer(t, "alice") },
		5*time.Second, 200*time.Millisecond)

	code := alice.RequestPairing(t, "bob")
	bob.ConfirmPairing(t, code)
	require.True(t, alice.WaitForPairedWith(t, 5*time.Second))

	client := dialSFTPAs(t, bob, alice)

	// Read side works.
	f, err := client.Open("/docs/hello.txt")
	require.NoError(t, err, "bob should be able to open ro share")
	defer f.Close()
	var buf bytes.Buffer
	_, err = buf.ReadFrom(f)
	require.NoError(t, err)
	assert.Equal(t, "hi", buf.String())

	// Write side rejected.
	_, err = client.Create("/docs/new.txt")
	assert.Error(t, err, "write to ro share must fail")
}
```

- [ ] **Step 2: Ensure `AgentSSHUser` is exported for tests**

Check `internal/common/`: if there is no `AgentSSHUser` constant, use the literal `"hubfuse"` (matches what `tests/scenarios/mount_test.go` already asserts: `marker.RemoteUser == "hubfuse"`). If the constant exists under another name, use that; do not introduce a new file for a single string.

If you must inline: replace `common.AgentSSHUser` with `"hubfuse"` and drop the `common` import.

- [ ] **Step 3: Run the new test**

Run: `go test ./tests/scenarios/... -run TestACL_ReadOnlyRejectsWrites -timeout 60s -v`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add tests/scenarios/permissions_test.go
git commit -m "test(scenarios): ro share accepts reads, rejects writes"
```

---

### Task 11: Scenario — `allowed_devices` hides shares from other peers

**Files:**
- Modify: `tests/scenarios/permissions_test.go`

- [ ] **Step 1: Append the scenario**

```go
// TestACL_AllowedDevicesFiltersListing — alice declares a share allowing only
// bob. bob sees it in the root listing; carol does not, and direct access is
// denied.
func TestACL_AllowedDevicesFiltersListing(t *testing.T) {
	hub := helpers.StartHub(t)
	exportDir := t.TempDir()
	require.NoError(t, writeTestFile(exportDir, "secret.txt", "s3cr3t"), "seed export")

	alice := helpers.StartAgent(t, hub, "alice", helpers.WithExportACL(exportDir, "docs", "ro", "bob"))
	alice.Join(t)
	alice.StartDaemon(t)

	bob := helpers.StartAgent(t, hub, "bob")
	bob.Join(t)
	bob.StartDaemon(t)

	carol := helpers.StartAgent(t, hub, "carol")
	carol.Join(t)
	carol.StartDaemon(t)

	require.Eventually(t,
		func() bool {
			return alice.HasPeer(t, "bob") && alice.HasPeer(t, "carol")
		},
		5*time.Second, 200*time.Millisecond)

	alice.ConfirmPairing(t, bob.RequestPairingWith(t, "alice", alice)) // see helper note below
	alice.ConfirmPairing(t, carol.RequestPairingWith(t, "alice", alice))

	require.True(t, alice.WaitForPairedCount(t, 2, 5*time.Second))

	// bob: share visible, read works.
	bobClient := dialSFTPAs(t, bob, alice)
	entries, err := bobClient.ReadDir("/")
	require.NoError(t, err)
	names := []string{}
	for _, e := range entries {
		names = append(names, e.Name())
	}
	assert.Contains(t, names, "docs", "bob should see docs in /")

	// carol: share must not appear; direct access denied.
	carolClient := dialSFTPAs(t, carol, alice)
	carolEntries, err := carolClient.ReadDir("/")
	require.NoError(t, err, "root listing itself still succeeds, just empty for carol")
	for _, e := range carolEntries {
		assert.NotEqual(t, "docs", e.Name(), "carol must not see docs")
	}
	_, err = carolClient.Open("/docs/secret.txt")
	assert.Error(t, err, "direct access by carol must be denied")
}
```

**Note on `RequestPairingWith` / `WaitForPairedCount`:** check whether equivalent helpers already exist in `tests/scenarios/helpers/agent.go`. If they don't, the simplest path is:
- Reuse `alice.RequestPairing(t, "bob")` and `bob.ConfirmPairing(t, code)` (initiated by alice, confirmed by bob — same order as `TestPairAndMountBasic`).
- Replace `WaitForPairedCount(t, 2, ...)` with two individual `WaitForPairedWith` calls (one after each pairing).

Rewrite the above block accordingly — do **not** invent new helpers for this task.

- [ ] **Step 2: Run the test**

Run: `go test ./tests/scenarios/... -run TestACL_AllowedDevicesFiltersListing -timeout 90s -v`
Expected: PASS.

- [ ] **Step 3: Commit**

```bash
git add tests/scenarios/permissions_test.go
git commit -m "test(scenarios): allowed_devices filters listing and blocks others"
```

---

### Task 12: Scenario — `"all"` wildcard works; default-deny bites

**Files:**
- Modify: `tests/scenarios/permissions_test.go`

- [ ] **Step 1: Append two more scenarios**

```go
// TestACL_WildcardAllowsEverybody — allowed-devices "all" means any paired peer.
func TestACL_WildcardAllowsEverybody(t *testing.T) {
	hub := helpers.StartHub(t)
	exportDir := t.TempDir()
	require.NoError(t, writeTestFile(exportDir, "pub.txt", "public"), "seed export")

	alice := helpers.StartAgent(t, hub, "alice", helpers.WithExportACL(exportDir, "pub", "ro", "all"))
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
	require.NoError(t, err)
	names := []string{}
	for _, e := range entries {
		names = append(names, e.Name())
	}
	assert.Contains(t, names, "pub", `"all" wildcard must show share to paired peer`)
}

// TestACL_DefaultDeny — a share with no allowed-devices (CLI omitted --allow)
// is invisible and inaccessible to any paired peer.
func TestACL_DefaultDeny(t *testing.T) {
	hub := helpers.StartHub(t)
	exportDir := t.TempDir()
	require.NoError(t, writeTestFile(exportDir, "private.txt", "nope"), "seed export")

	// Export with no --allow at all: permissions default to ro, allowed_devices empty.
	alice := helpers.StartAgent(t, hub, "alice", helpers.WithExportACL(exportDir, "private", "ro"))
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
```

- [ ] **Step 2: Run both**

Run: `go test ./tests/scenarios/... -run 'TestACL_Wildcard|TestACL_DefaultDeny' -timeout 60s -v`
Expected: PASS.

- [ ] **Step 3: Commit**

```bash
git add tests/scenarios/permissions_test.go
git commit -m "test(scenarios): \"all\" wildcard + default-deny behaviour"
```

---

### Task 13: Full test + vet sweep; README note

**Files:**
- Modify: `README.md`

- [ ] **Step 1: README update**

Locate the section that describes share configuration in `README.md`. Append a short paragraph:

```markdown
### Share access control

Each share declares who may access it and whether the access is read-only:

```kdl
shares {
    share "/home/user/photos" alias="photos" permissions="ro" {
        allowed-devices "laptop" "tablet"
    }
}
```

Defaults are secure: if `permissions` is omitted, the share is read-only; if
`allowed-devices` is missing or empty, the share is inaccessible to every peer.
Use the literal token `"all"` to grant access to every paired device, e.g.
`allowed-devices "all"`.
```

If the README has no existing shares section, insert the block after the
"Configuration" heading; keep it short — this is reference material, not a
tutorial.

- [ ] **Step 2: Run the full test suite**

Run (from the worktree):
```
go vet ./...
go test ./internal/... -timeout 60s
go test ./tests/integration/... -timeout 120s
go test ./tests/scenarios/... -timeout 180s
```
Expected: all PASS.

- [ ] **Step 3: Commit**

```bash
git add README.md
git commit -m "docs(readme): document secure-default ACL semantics"
```

- [ ] **Step 4: Push and open PR**

```bash
git push -u origin fix/issue-31-enforce-share-acl
gh pr create --title "fix(agent): enforce per-share ACL (closes #31)" --body "$(cat <<'EOF'
## Summary
- Replace `sftp.NewServer` with a per-connection `sftp.RequestServer` backed by a custom `sftp.Handlers` that knows the connecting peer's `device_id` and consults an atomically-swappable `[]ShareACL` snapshot.
- Propagate `device_id` through SSH `Permissions.Extensions` so the SFTP layer can identify the peer.
- `permissions="ro"` now rejects every SFTP write-class request; `allowed_devices` actually gates listing and direct access. Missing fields deny by default (this is a deliberate tightening — see #31).
- Drop the global `sftp-root` symlink tree; the handler translates virtual paths itself.

## Test plan
- [x] `go vet ./...`
- [x] `go test ./internal/... -timeout 60s`
- [x] `go test ./tests/integration/... -timeout 120s`
- [x] `go test ./tests/scenarios/... -timeout 180s`
- [x] New unit tests in `internal/agent/sharesacl_test.go`, `internal/agent/sftphandler_test.go`
- [x] New scenario tests in `tests/scenarios/permissions_test.go` cover ro-rejects-writes, allowed_devices filtering, `"all"` wildcard, default-deny.

Closes #31

🤖 Generated with [Claude Code](https://claude.com/claude-code)
EOF
)"
```

---

## Self-review

- **Spec coverage:** every spec section maps to a task —
  `Approach / SFTP layer` → Tasks 5–6; `Binding device_id` → Task 3;
  `ACL snapshot` → Task 4 + Task 7; `Matching allowed_devices` → Tasks 1–2;
  `Handlers behaviour` → Task 5; `Removed code` → Task 6; `Testing` →
  Tasks 1, 2, 5, 9–12; `Observability / migration` → Task 8 + Task 13;
  `Risks` surfaced in README in Task 13.
- **Placeholder scan:** no "TBD"/"fill in later" lines. Every step either
  shows the code to write or names the exact function/file to change with
  copy-pasteable snippets.
- **Type consistency:** `ShareACL`, `DeviceResolver`, `AccessDecision`,
  `shareConfigView`, `ShareACLsFromConfig`, `ResolveSharePath`,
  `aclHandlers`, `newACLHandlers`, `aclSnapshot`, `SetDeviceResolver`,
  `currentResolver`, `sharesToACL`, `NicknameForDeviceID`, and the
  `"hubfuse-device-id"` extension key are introduced in Task N and reused
  verbatim in later tasks.
