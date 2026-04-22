# Enforce per-share ACL (`permissions`, `allowed_devices`) — design

Closes: #31 (critical security)

## Problem

The SFTP server started by a HubFuse agent does not enforce the `permissions` and
`allowed_devices` fields from `config.kdl`. Any peer that has completed pairing
gets full read+write access to every share on the host, irrespective of what the
config declares. This is a silent mismatch between documented semantics and
actual behaviour.

Key evidence in the current tree:

- `internal/agent/sshserver.go:69-77` — `publicKeyCallback` only checks
  membership in `allowedKeyCache` (keyed by marshalled public key) and never
  resolves the key to a `device_id`.
- `internal/agent/sshserver.go:93-103` — `UpdateAllowedKeys` takes a
  `map[deviceID]ssh.PublicKey` but collapses it to a set of marshalled keys,
  dropping the `device_id` association.
- `internal/agent/sshserver.go:268-293` — `serveSFTP` constructs
  `sftp.NewServer(channel, WithServerWorkingDirectory(sftpRoot))`. There is no
  read-only option and no per-request filter.
- `internal/agent/confighandler.go:60-66` — `sharesToMap` produces
  `map[alias]path`, discarding `Permissions` and `AllowedDevices`.
- `internal/agent/sshserver.go:107-133` — `rebuildSFTPRoot` builds a single
  symlink directory listing **all** shares; every authenticated peer sees the
  same tree.

## Goals

1. A peer connecting over SFTP sees only the shares whose `allowed_devices`
   list includes its identity.
2. Shares declared `permissions="ro"` reject every SFTP write-class request
   from that peer.
3. Configuration semantics are explicit and secure by default — missing or
   empty ACL fields deny access rather than grant it.
4. The enforcement is covered by scenario tests that exercise two paired
   agents end to end.

## Non-goals

- No change to KDL syntax, proto definitions, or hub behaviour. ACL is a
  local agent concern.
- No change to how paired-device public keys are stored on disk.
- No chroot, no user-namespace tricks. Enforcement happens at the SFTP
  protocol layer.

## Approach

### SFTP layer

Replace `sftp.NewServer(...)` with `sftp.NewRequestServer(channel, handlers)`
from `github.com/pkg/sftp` v1.13.10 (already in `go.mod`). `NewRequestServer`
accepts a `sftp.Handlers` struct, which gives per-request visibility into
every file operation and per-connection state. That is the only mechanism in
this library that can enforce both visibility filtering and write blocking.

Each accepted SSH session creates a fresh `sftp.Handlers` bound to the
connecting peer's `device_id` and the current ACL snapshot. No per-process
`sftpRoot` directory is required anymore — virtual paths like `/<alias>/...`
are translated to real filesystem paths inside the handler.

The current `sftpRoot` mechanism (`rebuildSFTPRoot`, the symlink tree, the
`sftp-root` working directory) is removed. It is unsafe (no filtering) and
redundant once the request handler maps virtual paths itself.

### Binding `device_id` to the SSH session

`publicKeyCallback` will succeed only when the presented key matches a known
paired device. On success it returns
`*gossh.Permissions{Extensions: {"hubfuse-device-id": deviceID}}`. The
`device_id` is then read back in `handleConn` / `serveSFTP` via
`sshConn.Permissions().Extensions["hubfuse-device-id"]`. `gossh.Permissions`
is specifically designed for this hand-off.

To support this, `sshServer` stores a reverse index
`map[keyFingerprint]deviceID` where `keyFingerprint = string(key.Marshal())`,
populated alongside `allowedKeyCache` inside `UpdateAllowedKeys`.

### ACL snapshot

A new value type replaces the `map[alias]path` currently used by
`sshServer.UpdateShares`:

```go
type ShareACL struct {
    Alias          string
    Path           string
    ReadOnly       bool
    AllowAll       bool     // true when the config lists "all"
    AllowedDevices []string // raw tokens from KDL (nicknames or device_ids)
}
```

`sshServer` keeps an `atomic.Pointer[[]ShareACL]`. `UpdateShares` stores a new
snapshot on every config change. Each active handler dereferences the pointer
on every request, so fsnotify-driven hot reload takes effect on the next
operation without locking.

`confighandler.sharesToMap` is replaced by `sharesToACL`, which preserves
`Permissions` and `AllowedDevices`.

### Matching `allowed_devices`

Tokens in `allowed_devices` are matched against both the peer's **nickname**
and its **device_id**. Rationale: users write human-readable nicknames in
KDL, but a device may be referenced by UUID in automation. Matching both
costs nothing and avoids a foot-gun.

- `"all"` is a reserved wildcard: any paired device passes.
- Empty list / missing field: **deny**. This is a behavioural change from
  today's accidental allow-all, and is the whole point of the issue.
- `permissions` missing: treated as `"ro"`. Secure default; writes must be
  opted into explicitly.

The `sshServer` needs a way to resolve `deviceID → nickname`. The daemon
already maintains that mapping (device events from the hub). We add a
minimal interface:

```go
type DeviceResolver interface {
    NicknameForDeviceID(id string) (string, bool)
}
```

The daemon passes a resolver implementation to `NewSSHServer`. When the
resolver has no entry for a `device_id` (e.g. briefly after pairing, before
the first device event), matching falls back to device_id-only.

### Handlers behaviour

The custom handler implements `FileReader`, `FileWriter`, `FileCmder`, and
`FileLister`. For every incoming `*sftp.Request`:

1. If `req.Filepath == "/"`, `Filelist` returns a synthetic directory listing
   of shares visible to this `device_id` (ACL-allowed only). No other
   handler method accepts `/`.
2. Otherwise the first path segment is the alias. Look up the `ShareACL`:
   - Not found, or ACL denies this device → return `sftp.ErrSSHFxPermissionDenied`.
   - Method is write-class and `ReadOnly` is true → same error.
3. Translate the remaining path under the share's real `Path` and delegate
   to `os.Open` / `os.OpenFile` / `os.Rename` etc.

Write-class methods: `Put`, `Open` with write flags, `Setstat`, `Rename`,
`Remove`, `Rmdir`, `Mkdir`, `Symlink`, `Link`. The canonical source is
`sftp.Request.Method` — the handler inspects it directly.

Path safety: before touching the filesystem, the translated path is cleaned
and verified to remain underneath the share's root (reject `..` traversal).
Symlinks inside a share resolve normally; crossing out of the share's root
is denied at the Open/Stat boundary.

### Removed code

- `sftpRootDir` field, `rebuildSFTPRoot`, and all symlink-tree bookkeeping
  in `sshserver.go`.
- `WithServerWorkingDirectory` call.

## Testing

Scenario tests (`tests/scenarios/mount_permissions_test.go`) using the
existing two-agent pairing helpers:

1. **ro share rejects writes** — Agent A exports `foo` with `permissions="ro"`,
   `allowed-devices "B"`. Agent B pairs, mounts, reads existing file
   successfully, write attempt fails with `SSH_FX_PERMISSION_DENIED`.
2. **allowed_devices filters listing** — Agent A exports `foo` with
   `allowed-devices "B"`. Agents B and C pair with A. B sees `foo` in the
   root listing; C does not. C's direct access to `/foo/...` is denied.
3. **wildcard `"all"`** — Share with `allowed-devices "all"` is visible to
   every paired peer; acts as a regression guard for the wildcard path.
4. **default-deny** — Share without `allowed-devices` is invisible to every
   paired peer and rejects direct access. Agent start logs a warning for
   such shares.

Unit tests (`internal/agent/sshserver_acl_test.go`) cover the pure matcher
logic without an SSH server: device/nickname matching, `"all"` wildcard,
empty list denies, write-class method classification, path traversal
rejection.

Existing tests that rely on the old `sftpRoot` / `UpdateShares(map[...]string)`
signature are updated to the new `[]ShareACL` shape.

## Observability / migration

- At startup and on hot-reload, for every share with an empty
  `allowed-devices` list, log a WARN line:
  `share %q has no allowed-devices and will be inaccessible`.
- Add a short section to the README describing the new defaults and pointing
  to the `"all"` escape hatch for users who genuinely want the old behaviour.

## Risks

- **Breaking change** for anyone whose `config.kdl` omits `allowed-devices`
  or `permissions`. This is the explicit intent of the issue (current
  behaviour is a CVE-class bug). The warning log and README update mitigate
  surprise.
- **DeviceResolver staleness** immediately after pairing: a freshly paired
  peer whose nickname is not yet known will still match by `device_id`, and
  KDL-style nickname ACLs will temporarily reject it until the hub event
  arrives. Acceptable — pairing is not an access grant, only ACL membership
  is.

## Out of scope

- Per-subpath ACL inside a share.
- Audit logging of denied operations beyond standard error logging.
- Any rework of `known_devices` storage, or of the pairing flow itself.
