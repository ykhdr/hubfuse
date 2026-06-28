package agent

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	agentconfig "github.com/ykhdr/hubfuse/internal/agent/config"
)

// mountBackend describes what a selected mount tool needs to run a mount.
// It is the single source of truth for the binary to exec and any
// backend-specific -o options to append to the command. The configured tool
// name is the map key in mountBackends, so it is not duplicated here.
type mountBackend struct {
	binary    string   // executable to run
	extraOpts []string // backend-specific -o options appended to the command
}

// mountBackends maps a configured mount-tool value to its backend profile.
//
// Both profiles use the "sshfs" binary: macFUSE-sshfs and fuse-t-sshfs install
// to the same path and collide, so a device has exactly one "sshfs" on PATH.
// Every flag the mounter passes (-p, -o IdentityFile, -o StrictHostKeyChecking,
// -o UserKnownHostsFile) is an SSH option that sshfs forwards to ssh, not a
// FUSE option, so the same invocation works for either FUSE implementation.
//
// The fuse-t profile disables the sshfs-fork's internal attribute cache:
// FUSE-T's bundled sshfs (2.9-based) serves a stale size-0 stat to the NFS
// translation layer right after a create→write→close sequence, so the next
// append computes EOF as 0 and overwrites the file head (issue #45, diagnosed
// live). The NFS layer and the macOS client keep their own caches, so this
// only trades a redundant third cache for correctness.
var mountBackends = map[string]mountBackend{
	"sshfs":  {binary: "sshfs", extraOpts: nil},
	"fuse-t": {binary: "sshfs", extraOpts: []string{"cache=no"}}, // fuse-t ships a drop-in sshfs
}

// resolveBackend returns the backend profile for the configured mount tool.
// An empty value defaults to "sshfs"; an unknown value also falls back to the
// "sshfs" profile as a defensive default (config Load already rejects unknown
// values, so this only guards against programmer error).
func resolveBackend(tool string) mountBackend {
	if tool == "" {
		return mountBackends["sshfs"]
	}
	if b, ok := mountBackends[tool]; ok {
		return b
	}
	return mountBackends["sshfs"]
}

// sshfs reconnect/keepalive options. sshfs honours -o reconnect itself and
// forwards ServerAlive* to ssh: after sshKeepaliveCountMax unanswered probes
// spaced sshKeepaliveInterval seconds apart (~45s) the SSH session is detected
// dead, and reconnect transparently re-establishes it in the background. This
// heals a same-IP TCP blip (a brief network drop without an address change)
// without an unmount/remount — the daemon never sees it. (issue #61)
const (
	sshKeepaliveInterval = 15
	sshKeepaliveCountMax = 3
)

// buildMountArgs builds the argument list for the mount command. It emits the
// base SSH options (including the sshfs reconnect/keepalive options so a
// same-IP TCP blip self-heals; see sshKeepaliveInterval), then any
// backend-specific extraOpts as ordered "-o <opt>" pairs, and finally the
// "hubfuse@<ip>:<share>" source and "<to>" target operands last (their
// position is significant to sshfs).
func buildMountArgs(b mountBackend, sshPort int, keyPath, knownHosts, deviceIP, share, to string) []string {
	args := []string{
		"-p", strconv.Itoa(sshPort),
		"-o", "IdentityFile=" + keyPath,
		"-o", "StrictHostKeyChecking=yes",
		"-o", "UserKnownHostsFile=" + knownHosts,
		"-o", "reconnect",
		"-o", "ServerAliveInterval=" + strconv.Itoa(sshKeepaliveInterval),
		"-o", "ServerAliveCountMax=" + strconv.Itoa(sshKeepaliveCountMax),
	}
	for _, opt := range b.extraOpts {
		args = append(args, "-o", opt)
	}
	args = append(args, "hubfuse@"+deviceIP+":"+share, to)
	return args
}

// validateMountTool validates a configured mount-tool value against the target
// OS. Unknown values are rejected on any OS. "fuse-t" is rejected unless goos
// is "darwin", since FUSE-T is macOS-only. An empty value is treated as the
// default "sshfs" and accepted.
func validateMountTool(tool, goos string) error {
	switch tool {
	case "", "sshfs":
		return nil
	case "fuse-t":
		if goos != "darwin" {
			return fmt.Errorf("mount-tool %q is only supported on macOS", "fuse-t")
		}
		return nil
	default:
		return fmt.Errorf("invalid mount-tool %q: must be \"sshfs\" or \"fuse-t\"", tool)
	}
}

// isMountpoint reports whether path is a mountpoint: it stats both path and
// its parent directory and returns true when their device IDs differ — a FUSE
// (or any other) mount overlays path with a new filesystem whose st_dev is
// distinct from the parent's. Uses only stdlib syscall; adds no dependencies.
// This is unix-only; the mounter already depends on UNIX-specific unmount
// helpers (umount / fusermount).
func isMountpoint(path string) (bool, error) {
	var st, parentSt syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		return false, err
	}
	parent := filepath.Dir(path)
	if err := syscall.Stat(parent, &parentSt); err != nil {
		return false, err
	}
	return st.Dev != parentSt.Dev, nil
}

// unmountLadder returns the ordered list of argv sequences to try for an
// unmount operation. Each inner slice is a complete command (binary + args).
// The caller should try them in order, stopping on first success. This is a
// pure helper — it has no side effects and is the single source of truth for
// the per-OS escalation strategy. It mirrors the design of buildMountArgs so
// it is table-testable without requiring real FUSE mounts. (#50 bounded/force)
//
// Linux non-force: fusermount -u → fusermount -uz (lazy)
// Linux force:     fusermount -u → fusermount -uz → umount -l
// macOS non-force: umount
// macOS force:     umount → diskutil unmount force → umount -f
func unmountLadder(goos string, force bool) [][]string {
	switch goos {
	case "darwin":
		ladder := [][]string{
			{"umount"},
		}
		if force {
			ladder = append(ladder,
				[]string{"diskutil", "unmount", "force"},
				[]string{"umount", "-f"},
			)
		}
		return ladder
	default:
		ladder := [][]string{
			{"fusermount", "-u"},
			{"fusermount", "-uz"},
		}
		if force {
			ladder = append(ladder, []string{"umount", "-l"})
		}
		return ladder
	}
}

// unmountPath runs the platform-specific unmount command for path.
// It uses exec.CommandContext so a wedged command is abandoned when ctx is
// cancelled or times out. force selects the escalation strategy (see
// unmountLadder). Returns nil on first success; aggregates the last error
// otherwise. (#50 bounded/force)
func unmountPath(ctx context.Context, path string, force bool) error {
	steps := unmountLadder(runtime.GOOS, force)
	var lastErr error
	for _, argv := range steps {
		cmd := exec.CommandContext(ctx, argv[0], append(argv[1:], path)...)
		out, err := cmd.CombinedOutput()
		if err == nil {
			return nil
		}
		lastErr = fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
		// Stop early if the context is cancelled — no point trying further rungs.
		if ctx.Err() != nil {
			return lastErr
		}
	}
	return lastErr
}

// mountKey is the map key for an active mount, uniquely identifying a
// device+share pair without string concatenation.
type mountKey struct {
	Device string
	Share  string
}

// Mount represents an active SSHFS mount.
type Mount struct {
	Device    string
	Share     string
	LocalPath string
	IP        string
	SSHPort   int
	cmd       *exec.Cmd
}

// guardMode is the restricted mode applied to an unmounted mount target:
// r-x for owner, nothing for group/other. The cleared write bit blocks entry
// creation/deletion (so stray local writes fail with EACCES); the set execute
// bit still lets the agent traverse the path. A live FUSE mount masks this mode
// while active, and it is re-applied on unmount/reap.
const guardMode os.FileMode = 0o500

// mountableMode is applied to the mount point immediately before invoking the
// mount backend. fusermount3 (Linux) refuses to mount onto a directory the user
// cannot write to — and guardMode (0o500) clears the write bit — so the point is
// briefly made owner-writable for the mount to attach. A successful mount masks
// this mode; every failure path (and a later unmount) re-applies guardMode, so
// the target is only this permissive during the bounded mount-establishment
// window. (macFUSE/FUSE-T tolerate 0o500, but Linux fusermount3 does not, so we
// chmod unconditionally for cross-platform correctness.)
const mountableMode os.FileMode = 0o700

// Mounter manages SSHFS mounts for remote shares.
type Mounter struct {
	keyPath         string       // path to agent's SSH private key
	knownDevicesDir string       // dir containing paired-peer public keys (<device_id>.pub)
	knownHostsDir   string       // dir where per-mount SSH known_hosts files are written
	backend         mountBackend // selected mount tool profile (binary + extra opts)
	logger          *slog.Logger
	activeMounts    map[mountKey]*Mount
	mu              sync.Mutex

	// stub is true when HUBFUSE_STUB_MOUNT_DIR is set (scenario-test harness).
	// guardTarget/unguardTarget/targetHasLocalContents are no-ops under the stub:
	// stub-sshfs never creates a real FUSE mount to mask the 0o500 mode, so a
	// lingering restricted dir would be pointless and confuse scenario tests.
	// (Note: stub-sshfs writes to HUBFUSE_STUB_MOUNT_DIR, never through the target
	// path, so no scenario test writes to the raw target dir.)
	stub bool

	// execCommand is used to build commands; override in tests.
	execCommand func(ctx context.Context, name string, args ...string) *exec.Cmd
	// unmount is used to unmount a path; override in tests. (#50 bounded/force)
	unmount func(ctx context.Context, path string, force bool) error
	// checkMountpoint reports whether path is currently a mountpoint. Defaults to
	// isMountpoint; override in tests (or when stub-sshfs is in use) to skip the
	// real filesystem check.
	checkMountpoint func(path string) (bool, error)
	// mountVerifyTimeout is the maximum time to wait for the mountpoint to appear
	// after cmd.Start() returns. Defaults to 10 seconds.
	mountVerifyTimeout time.Duration
	// mountVerifyInterval is the polling interval for mountpoint checks.
	// Defaults to 200ms.
	mountVerifyInterval time.Duration
}

// NewMounter creates a new Mounter. mountTool selects the mount backend
// ("sshfs" default, or "fuse-t"); an empty or unknown value falls back to the
// "sshfs" profile (see resolveBackend).
//
// When HUBFUSE_STUB_MOUNT_DIR is set (the scenario-test harness that uses
// stub-sshfs, which never creates a real FUSE mountpoint), mountpoint
// verification is bypassed so that scenario tests do not time out waiting for
// a filesystem that the stub intentionally never creates.
func NewMounter(keyPath, knownDevicesDir, knownHostsDir, mountTool string, logger *slog.Logger) *Mounter {
	stubMode := os.Getenv("HUBFUSE_STUB_MOUNT_DIR") != ""
	check := isMountpoint
	if stubMode {
		// Stub harness: skip real mountpoint verification.
		check = func(string) (bool, error) { return true, nil }
	}
	return &Mounter{
		keyPath:             keyPath,
		knownDevicesDir:     knownDevicesDir,
		knownHostsDir:       knownHostsDir,
		backend:             resolveBackend(mountTool),
		logger:              logger,
		activeMounts:        make(map[mountKey]*Mount),
		stub:                stubMode,
		execCommand:         exec.CommandContext,
		unmount:             unmountPath,
		checkMountpoint:     check,
		mountVerifyTimeout:  10 * time.Second,
		mountVerifyInterval: 200 * time.Millisecond,
	}
}

// Mount mounts the remote share described by mc from deviceIP:sshPort using SSHFS.
// Callers are responsible for ensuring the device is paired before calling Mount;
// the peer's public key stored at <knownDevicesDir>/<deviceID>.pub is pinned as
// the only accepted SSH host key for the connection.
func (m *Mounter) Mount(ctx context.Context, mc agentconfig.MountConfig, deviceID, deviceIP string, sshPort int) error {
	key := mountKey{Device: mc.Device, Share: mc.Share}

	m.mu.Lock()
	defer m.mu.Unlock()

	// An existing mount for this key is not necessarily an error. If the peer's
	// endpoint is unchanged it is a silent no-op; if the peer roamed to a new
	// IP/port we tear the stale mount down and re-mount at the new endpoint — all
	// under the same m.mu so a concurrent tryMount (config-watcher goroutine) and
	// handleDeviceOnline (supervise goroutine) can never interleave a
	// check-and-remount, and all four Mount call sites get this for free. (#61)
	if existing, exists := m.activeMounts[key]; exists {
		if existing.IP == deviceIP && existing.SSHPort == sshPort {
			// Same endpoint — the live mount already points at the right place.
			// Return BEFORE guardTarget so a live mount's masked mode is never
			// clobbered.
			return nil
		}
		// Peer roamed (DHCP address change / SSH port change). Tear down the stale
		// mount pointing at the now-dead old endpoint, then fall through to the
		// normal mount flow to attach a fresh mount at the new endpoint.
		m.logger.Info("re-mounting peer at new endpoint",
			"device", mc.Device,
			"share", mc.Share,
			"old_ip", existing.IP,
			"old_port", existing.SSHPort,
			"new_ip", deviceIP,
			"new_port", sshPort,
		)
		// force=true: the old endpoint is most likely unreachable, so the unmount
		// must escalate (the force ladder reaches umount -l). reguard=false: the
		// normal flow below re-guards the target via guardTarget. Bound the
		// unmount with unmountOpTimeout — this remount path has no caller deadline.
		rctx, cancel := context.WithTimeout(ctx, unmountOpTimeout)
		err := m.unmountKey(rctx, key, true, false) // force=true, reguard=false
		cancel()
		if err != nil {
			// Could not tear down the stale mount — do NOT start a new one; the
			// stale entry is retained (by unmountKey) for a later retry.
			return fmt.Errorf("re-mount %q from device %q: unmount stale endpoint %s:%d: %w",
				mc.Share, mc.Device, existing.IP, existing.SSHPort, err)
		}
		// Stale entry removed; continue into the normal mount flow below.
	}

	// Create the mount point directory (if needed) and restrict it immediately so
	// even the create→mount window is guarded. This runs only AFTER the
	// already-mounted check above, so we never chmod a path that is currently a
	// live mount of ours. (#49 guard-target)
	if err := m.guardTarget(mc.To); err != nil {
		return fmt.Errorf("guard mount point %q: %w", mc.To, err)
	}

	// Refuse to mount over a non-empty local directory — doing so would silently
	// shadow the local contents, which is the #49 failure mode in reverse and
	// risks data loss. (#49 non-empty refusal)
	entryCount, contentsErr := m.targetHasLocalContents(mc.To)
	if contentsErr != nil {
		return fmt.Errorf("check mount target %q for local contents: %w", mc.To, contentsErr)
	}
	if entryCount > 0 {
		m.logger.Warn("refusing to mount over a non-empty local directory — local files would be shadowed",
			"path", mc.To,
			"entry_count", entryCount,
			"remedy", fmt.Sprintf("move or remove the local files in %q, or change the 'to' path in your config, then retry", mc.To),
		)
		return fmt.Errorf("mount target %q contains %d local file(s); move/remove them or choose a different 'to' path", mc.To, entryCount)
	}

	// Materialise known_hosts under the lock so concurrent Mounts for the same
	// device cannot race-clobber each other, and so the same-endpoint early-return
	// above cannot leave a rewritten file on disk.
	knownHostsPath, err := m.writeKnownHostsFile(deviceID, deviceIP, sshPort)
	if err != nil {
		return err
	}

	// Make the point writable so the mount backend can attach (fusermount3 on
	// Linux requires it). The live mount masks this; every failure path below and
	// a later unmount re-apply guardMode. (#49 guard-target)
	if err := m.makeMountable(mc.To); err != nil {
		// Still guarded at guardMode (chmod failed) — safe; just report.
		return fmt.Errorf("mount %q from device %q: %w", mc.Share, mc.Device, err)
	}

	// The remote path is just the alias; the SSH server maps aliases to real paths.
	args := buildMountArgs(m.backend, sshPort, m.keyPath, knownHostsPath, deviceIP, mc.Share, mc.To)
	cmd := m.execCommand(ctx, m.backend.binary, args...)

	if err := cmd.Start(); err != nil {
		// The point was made mountable above; re-restrict it so the failed mount
		// does not leave a writable target. (#49 guard-target)
		if guardErr := m.guardTarget(mc.To); guardErr != nil {
			m.logger.Warn("re-guard mount target after start failure", "path", mc.To, "error", guardErr)
		}
		return fmt.Errorf("start %s for %q from device %q: %w", m.backend.binary, mc.Share, mc.Device, err)
	}

	// Poll until the mountpoint appears (or timeout/ctx cancellation). The lock
	// is held throughout — mounts are serialised by the daemon event loop so a
	// worst-case mountVerifyTimeout hold is acceptable and avoids races between
	// concurrent Mount calls on the same key.
	deadline := time.Now().Add(m.mountVerifyTimeout)
	var lastCheckErr error
	for {
		if ctx.Err() != nil {
			// Context cancelled while waiting — kill and reap the backend.
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			// Re-restrict the target: the failed mount may have left the dir
			// traversable; re-guard so stray writes are blocked. (#49 guard-target)
			if guardErr := m.guardTarget(mc.To); guardErr != nil {
				m.logger.Warn("re-guard mount target after ctx-cancel", "path", mc.To, "error", guardErr)
			}
			return fmt.Errorf("mount %q from device %q at %q: %w", mc.Share, mc.Device, mc.To, ctx.Err())
		}

		ok, checkErr := m.checkMountpoint(mc.To)
		if checkErr != nil {
			lastCheckErr = checkErr
		}
		if ok {
			break
		}

		if time.Now().After(deadline) {
			// Timed out — kill and reap the backend process.
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			// Re-restrict the target after a failed mount. (#49 guard-target)
			if guardErr := m.guardTarget(mc.To); guardErr != nil {
				m.logger.Warn("re-guard mount target after verify timeout", "path", mc.To, "error", guardErr)
			}
			reason := "mountpoint did not appear"
			if lastCheckErr != nil {
				reason = lastCheckErr.Error()
			}
			return fmt.Errorf("mount %q from device %q did not appear at %q within %s: %s",
				mc.Share, mc.Device, mc.To, m.mountVerifyTimeout, reason)
		}

		time.Sleep(m.mountVerifyInterval)
	}

	m.activeMounts[key] = &Mount{
		Device:    mc.Device,
		Share:     mc.Share,
		LocalPath: mc.To,
		IP:        deviceIP,
		SSHPort:   sshPort,
		cmd:       cmd,
	}

	m.logger.Info("mounted share",
		"device", mc.Device,
		"share", mc.Share,
		"local_path", mc.To,
		"ip", deviceIP,
		"port", sshPort,
	)

	return nil
}

// writeKnownHostsFile materialises a per-device SSH known_hosts file pinning
// the peer's public key (saved during pairing) to its current endpoint. The
// returned path is passed to sshfs via UserKnownHostsFile, so the mount
// connection aborts on host-key mismatch instead of trusting the network.
func (m *Mounter) writeKnownHostsFile(deviceID, deviceIP string, sshPort int) (string, error) {
	if m.knownDevicesDir == "" || m.knownHostsDir == "" {
		return "", fmt.Errorf("mounter: known_devices/known_hosts directories not configured")
	}

	if err := validateDeviceID(deviceID); err != nil {
		return "", err
	}

	pubKey, err := LoadPeerPublicKey(m.knownDevicesDir, deviceID)
	if err != nil {
		return "", fmt.Errorf("load peer public key for device %q: %w", deviceID, err)
	}

	if err := os.MkdirAll(m.knownHostsDir, 0700); err != nil {
		return "", fmt.Errorf("create known_hosts dir %q: %w", m.knownHostsDir, err)
	}

	hostPattern := deviceIP
	if sshPort != 22 {
		hostPattern = fmt.Sprintf("[%s]:%d", deviceIP, sshPort)
	}
	line := fmt.Sprintf("%s %s\n", hostPattern, strings.TrimRight(pubKey, "\n"))

	path := filepath.Join(m.knownHostsDir, deviceID)
	if err := os.WriteFile(path, []byte(line), 0600); err != nil {
		return "", fmt.Errorf("write known_hosts %q: %w", path, err)
	}

	return path, nil
}

// mountpointGoneCtx runs m.checkMountpoint(path) in a goroutine and returns true
// ("gone / treat as unmounted") ONLY on positive evidence that the mount is no
// longer there:
//   - the check returns (false, _) — path is no longer a mountpoint, or
//   - the check returns (_, non-nil err) — path is inaccessible (ENOENT etc.),
//     which we treat as gone (favor self-heal over a stuck entry).
//
// If ctx is cancelled before the check returns, it returns FALSE — "could not
// confirm gone." A wedged syscall.Stat on a FUSE mount must not re-block a
// bounded teardown (#50), but a timeout is NOT evidence the mount is gone, so we
// must NOT reap on it: a runtime force caller (e.g. UnmountDevice) that simply
// hit its budget would otherwise drop a still-live mount and desync the in-memory
// state. The caller (unmountKey) turns a non-confirmed result into a retained
// entry + returned error; the goroutine is abandoned (acceptable: short timeout
// or the process is exiting). (#50 bounded, no false reap)
func (m *Mounter) mountpointGoneCtx(ctx context.Context, path string) bool {
	type result struct {
		isMnt bool
		err   error
	}
	ch := make(chan result, 1)
	go func() {
		isMnt, err := m.checkMountpoint(path)
		ch <- result{isMnt, err}
	}()
	select {
	case <-ctx.Done():
		// ctx fired before the check returned — we could NOT confirm the mount is
		// gone. Return false so the entry is retained and an error is reported;
		// the bounded caller still returns promptly (no hang). (#50 bounded)
		m.logger.Warn("mountpoint check did not complete within deadline; not reaping", "path", path)
		return false
	case r := <-ch:
		if r.err != nil {
			// Any error (ENOENT, EACCES, EINTR) is treated as "gone" to favour
			// self-healing over a permanently stuck entry (#47 reap). A transient
			// EACCES could theoretically reap a live mount; we accept that trade-off.
			return true
		}
		return !r.isMnt
	}
}

// unmountOpTimeout bounds a single unmount command + re-check in teardown paths
// that have no overall caller deadline (the interactive UnmountAll). The
// shutdown path (UnmountAllForce) passes its own already-bounded context, which
// is respected as-is so the total budget is not exceeded. (#50 bounded)
const unmountOpTimeout = 5 * time.Second

// reapMountCmd reaps a finished mount's backend process. sshfs daemonizes — it
// forks and the parent we Start()ed exits 0 once the mount is up — so by the
// time a mount is torn down (or reaped as dead) mnt.cmd is a long-exited zombie
// and Wait() returns immediately; it therefore never blocks the bounded teardown
// (#50). Without this reap a long-lived, frequently-roaming daemon (#61 remounts
// on every endpoint change unmount this path) would accumulate unreaped child
// processes. mnt.cmd is never read after Mount stores it, so reaping here cannot
// race another reader.
func reapMountCmd(mnt *Mount) {
	if mnt == nil || mnt.cmd == nil {
		return
	}
	_ = mnt.cmd.Wait()
}

// unmountKey is the core unmount implementation. It calls m.unmount(ctx, path,
// force) and, on failure, re-checks whether the path is still a mountpoint via
// mountpointGoneCtx. If the path is confirmed gone, the entry is reaped — deleted
// from activeMounts, a WARN is logged, and nil is returned (#47 dead-mount reap).
// If the command failed and the path is still a mountpoint — OR the re-check
// could not confirm within its deadline — the entry is RETAINED and an error is
// returned, so a bounded caller never hangs (#50) yet a still-live mount is never
// silently dropped.
//
// reguard controls whether to re-restrict the target dir to guardMode after the
// entry is removed (#49 guard-target). Pass reguard=true for all interactive and
// device-offline paths (the target stays in config and must be re-restricted so
// stray writes are blocked until the next mount). Pass reguard=false for two
// callers: shutdown (UnmountAllForce), where the process is exiting and the chmod
// is pointless; and the remount path (Mount's endpoint-change branch), where the
// normal mount flow that immediately follows re-guards the target itself, so a
// reguard here would be redundant work instantly undone. reguard failures are
// logged at WARN and never returned — a re-guard error must not turn a successful
// unmount into a failure.
//
// The caller must hold m.mu.
func (m *Mounter) unmountKey(ctx context.Context, key mountKey, force, reguard bool) error {
	mnt, exists := m.activeMounts[key]
	if !exists {
		return fmt.Errorf("no active mount for device %q share %q", key.Device, key.Share)
	}

	cmdErr := m.unmount(ctx, mnt.LocalPath, force)
	if cmdErr != nil {
		// Re-check whether the mountpoint is still present. #47 reap
		//
		// The force/shutdown path deliberately shares the caller's ctx so the
		// command + re-check together stay inside the single bounded budget
		// (#50). The interactive (force=false) path instead gives the re-check
		// its own fresh, independent 3s window: a slow-but-killed unmount
		// command must not starve the re-check into a false "gone" verdict that
		// reaps a still-live mount.
		recheckCtx := ctx
		if !force {
			var cancel context.CancelFunc
			recheckCtx, cancel = context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
		}
		if m.mountpointGoneCtx(recheckCtx, mnt.LocalPath) {
			// Mount is already gone — reap the stale entry and its backend
			// process, then return success.
			delete(m.activeMounts, key)
			reapMountCmd(mnt)
			m.logger.Warn("reaped dead mount entry",
				"device", key.Device,
				"share", key.Share,
				"local_path", mnt.LocalPath,
				"unmount_err", cmdErr,
			)
			if reguard {
				if err := m.guardTarget(mnt.LocalPath); err != nil {
					m.logger.Warn("re-guard mount target after reap", "path", mnt.LocalPath, "error", err)
				}
			}
			return nil
		}
		// Path is still a mountpoint, or the re-check could not confirm it is gone
		// within the deadline — treat as a real failure and retain the entry so a
		// retry is possible (never silently drop a possibly-live mount). (#50)
		return fmt.Errorf("unmount %q (device %q share %q): %w", mnt.LocalPath, key.Device, key.Share, cmdErr)
	}

	// Command succeeded — do NOT re-check (lazy unmount may still look mounted
	// briefly). Delete the entry, reap the backend process, and log.
	delete(m.activeMounts, key)
	reapMountCmd(mnt)
	m.logger.Info("unmounted share",
		"device", key.Device,
		"share", key.Share,
		"local_path", mnt.LocalPath,
	)
	if reguard {
		if err := m.guardTarget(mnt.LocalPath); err != nil {
			m.logger.Warn("re-guard mount target after unmount", "path", mnt.LocalPath, "error", err)
		}
	}
	return nil
}

// Unmount unmounts the share identified by device and share name.
// This is the back-compat interactive path: force=false, reguard=true. The 3s
// timeout bounds the unmount command itself; unmountKey gives the post-failure
// re-check its own independent window, so the interactive call can never hang
// indefinitely. (#47 reap, back-compat; #50 bounded; #49 guard-target)
func (m *Mounter) Unmount(device, share string) error {
	key := mountKey{Device: device, Share: share}

	// Bound the unmount command so the interactive path can never hang
	// indefinitely on a wedged mount. (#50 bounded)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	m.mu.Lock()
	defer m.mu.Unlock()

	return m.unmountKey(ctx, key, false, true) // force=false, reguard=true
}

// unmountAll is the core loop: iterates over all active mounts (snapshot taken
// under the lock) and calls unmountKey for each, accumulating errors.
//
// If ctx already carries a deadline (the shutdown path, UnmountAllForce), that
// single budget is shared across every mount as-is. If it does NOT (the
// interactive UnmountAll, which passes context.Background()), each mount is given
// its own unmountOpTimeout so one wedged mount can never hang the whole sweep
// indefinitely. (#50 bounded)
//
// reguard is threaded through to unmountKey — see unmountKey for semantics.
// (#49 guard-target)
func (m *Mounter) unmountAll(ctx context.Context, force, reguard bool) error {
	m.mu.Lock()
	keys := make([]mountKey, 0, len(m.activeMounts))
	for k := range m.activeMounts {
		keys = append(keys, k)
	}
	m.mu.Unlock()

	_, hasDeadline := ctx.Deadline()

	var errs []string
	for _, key := range keys {
		opCtx := ctx
		var cancel context.CancelFunc
		if !hasDeadline {
			opCtx, cancel = context.WithTimeout(ctx, unmountOpTimeout)
		}

		m.mu.Lock()
		err := m.unmountKey(opCtx, key, force, reguard)
		m.mu.Unlock()

		if cancel != nil {
			cancel()
		}
		if err != nil {
			errs = append(errs, err.Error())
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("unmount errors: %s", strings.Join(errs, "; "))
	}
	return nil
}

// UnmountAll unmounts all active mounts (interactive, force=false, reguard=true).
// Each mount is bounded by unmountOpTimeout (unmountAll adds a per-mount deadline
// because the background ctx has none) so a wedged mount cannot hang the sweep.
// (#50 bounded; #49 guard-target)
func (m *Mounter) UnmountAll() error {
	return m.unmountAll(context.Background(), false, true)
}

// UnmountAllForce unmounts all active mounts with force=true under the provided
// context. Used by daemon.Shutdown to guarantee a bounded teardown. reguard=false
// because the process is exiting and there is no benefit to chmoding the dirs.
// (#50 bounded/force; #49 guard-target)
func (m *Mounter) UnmountAllForce(ctx context.Context) error {
	return m.unmountAll(ctx, true, false)
}

// UnmountDevice unmounts all shares from the named device (force=true, reguard=true,
// because device-offline/-removed teardown should never leave wedged mounts behind,
// and the target stays in config so it must be re-restricted). (#50 force; #49 guard-target)
func (m *Mounter) UnmountDevice(deviceNickname string) error {
	m.mu.Lock()
	var keys []mountKey
	for k := range m.activeMounts {
		if k.Device == deviceNickname {
			keys = append(keys, k)
		}
	}
	m.mu.Unlock()

	// Short timeout so device-offline handling cannot wedge the event loop.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var errs []string
	for _, key := range keys {
		m.mu.Lock()
		err := m.unmountKey(ctx, key, true, true) // force=true: device is gone (#50 force); reguard=true (#49)
		m.mu.Unlock()
		if err != nil {
			errs = append(errs, err.Error())
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("unmount device %q errors: %s", deviceNickname, strings.Join(errs, "; "))
	}
	return nil
}

// IsActive reports whether the share from device is currently mounted.
func (m *Mounter) IsActive(device, share string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.activeMounts[mountKey{Device: device, Share: share}]
	return ok
}

// ActiveMounts returns a snapshot of all currently active mounts.
func (m *Mounter) ActiveMounts() []*Mount {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make([]*Mount, 0, len(m.activeMounts))
	for _, mnt := range m.activeMounts {
		result = append(result, mnt)
	}
	return result
}

// guardTarget creates the target directory if absent and chmods it to guardMode
// so an unmounted target cannot silently absorb local writes (EACCES on entry
// creation/deletion). It is idempotent. (#49 guard-target)
//
// guardTarget is only ever called on paths the caller has confirmed are currently
// unmounted (pre-mount before cmd.Start, on Mount failure, after delete in
// unmountKey, in the startup sweep, in tryMount's not-mounted branch). Relying on
// call-site ordering avoids an extra checkMountpoint call here, which could hang
// on a wedged FUSE stat. As a result, guardTarget does NOT call checkMountpoint.
//
// No-op when m.stub is true (scenario-test harness — see Mounter.stub).
func (m *Mounter) guardTarget(path string) error {
	if m.stub {
		return nil
	}
	// Create parent dirs at a normal mode so siblings are not affected.
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create parent of mount target %q: %w", path, err)
	}
	// Ensure the leaf exists at a normal mode first, then restrict it.
	if err := os.MkdirAll(path, 0o755); err != nil {
		return fmt.Errorf("create mount target %q: %w", path, err)
	}
	if err := os.Chmod(path, guardMode); err != nil {
		return fmt.Errorf("restrict mount target %q: %w", path, err)
	}
	return nil
}

// makeMountable chmods the mount point to mountableMode immediately before the
// mount backend attaches, because fusermount3 (Linux) refuses to mount onto a
// directory the user cannot write. A successful mount masks this mode; the
// caller re-applies guardMode on every failure path and on unmount. No-op when
// m.stub is true (scenario-test harness — the stub never creates a real mount).
// (#49 guard-target)
func (m *Mounter) makeMountable(path string) error {
	if m.stub {
		return nil
	}
	if err := os.Chmod(path, mountableMode); err != nil {
		return fmt.Errorf("prepare mount point %q for mounting: %w", path, err)
	}
	return nil
}

// unguardTarget restores the target directory to a normal mode (0o755) so a path
// the user has removed from config behaves like an ordinary directory again.
// (#49 guard-target)
//
// No-op when m.stub is true (scenario-test harness — see Mounter.stub).
func (m *Mounter) unguardTarget(path string) error {
	if m.stub {
		return nil
	}
	if err := os.Chmod(path, 0o755); err != nil {
		// Ignore ENOENT — if the directory does not exist there is nothing to restore.
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("restore mount target %q: %w", path, err)
	}
	return nil
}

// targetHasLocalContents returns the number of entries in path — i.e. the count
// of real local files that a mount would shadow. (#49 non-empty refusal)
//
// Returns 0 when the directory does not exist or exists but is empty, and the
// entry count otherwise. Returning the count (rather than a bool) lets the
// caller log/report it without a second os.ReadDir.
//
// targetHasLocalContents does NOT call checkMountpoint. It is only called
// pre-mount in Mount, before our mount exists, so os.ReadDir reflects the real
// local contents. Enumerating a pre-existing foreign mountpoint and refusing to
// mount over it is the correct, desired behavior.
//
// Returns 0 when m.stub is true so the non-empty refusal never trips in scenario
// tests (see Mounter.stub).
func (m *Mounter) targetHasLocalContents(path string) (int, error) {
	if m.stub {
		return 0, nil
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("read mount target %q: %w", path, err)
	}
	return len(entries), nil
}

// SetExecCommandForTests overrides the command builder (used in tests).
func (m *Mounter) SetExecCommandForTests(fn func(ctx context.Context, name string, args ...string) *exec.Cmd) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.execCommand = fn
}

// SetUnmountForTests overrides the unmount implementation (used in tests).
// The new signature matches the updated seam: (ctx, path, force). (#50 bounded/force)
func (m *Mounter) SetUnmountForTests(fn func(ctx context.Context, path string, force bool) error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.unmount = fn
}

// SetMountpointCheckForTests overrides the mountpoint check (used in tests and
// in the stub-sshfs scenario harness where a real FUSE mount is never created).
func (m *Mounter) SetMountpointCheckForTests(fn func(string) (bool, error)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.checkMountpoint = fn
}
