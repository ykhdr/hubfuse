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
// extraOpts is empty for both today; the field exists so a backend can inject
// FUSE-specific options later without touching the call site.
var mountBackends = map[string]mountBackend{
	"sshfs":  {binary: "sshfs", extraOpts: nil},
	"fuse-t": {binary: "sshfs", extraOpts: nil}, // fuse-t ships a drop-in sshfs
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

// buildMountArgs builds the argument list for the mount command. It emits the
// base SSH options, then any backend-specific extraOpts as ordered "-o <opt>"
// pairs, and finally the "hubfuse@<ip>:<share>" source and "<to>" target
// operands last (their position is significant to sshfs).
func buildMountArgs(b mountBackend, sshPort int, keyPath, knownHosts, deviceIP, share, to string) []string {
	args := []string{
		"-p", strconv.Itoa(sshPort),
		"-o", "IdentityFile=" + keyPath,
		"-o", "StrictHostKeyChecking=yes",
		"-o", "UserKnownHostsFile=" + knownHosts,
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
// bit still lets the agent traverse the path to mount over it. A live FUSE
// mount masks this mode while active, and it is re-applied on unmount/reap.
const guardMode os.FileMode = 0o500

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

	if _, exists := m.activeMounts[key]; exists {
		return fmt.Errorf("share %q from device %q is already mounted", mc.Share, mc.Device)
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
	// device cannot race-clobber each other, and so a duplicate-mount rejection
	// above cannot leave a rewritten file on disk.
	knownHostsPath, err := m.writeKnownHostsFile(deviceID, deviceIP, sshPort)
	if err != nil {
		return err
	}

	// The remote path is just the alias; the SSH server maps aliases to real paths.
	args := buildMountArgs(m.backend, sshPort, m.keyPath, knownHostsPath, deviceIP, mc.Share, mc.To)
	cmd := m.execCommand(ctx, m.backend.binary, args...)

	if err := cmd.Start(); err != nil {
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

// mountpointGoneCtx runs m.checkMountpoint(path) in a goroutine and returns
// true ("gone / treat as unmounted") if:
//   - the check returns (false, _) — path is no longer a mountpoint, or
//   - the check returns (_, non-nil err) — path is inaccessible (ENOENT etc.),
//     which we treat as gone (favor self-heal over a stuck entry), or
//   - ctx is cancelled before the check returns — a wedged syscall.Stat on a
//     FUSE mount must not re-block a bounded shutdown (#50).
//
// When ctx fires first the goroutine is abandoned (acceptable: process is exiting
// or the caller uses a short timeout). A one-liner comment inside marks the
// intentional checkErr-as-gone behavior.
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
		// ctx fired — treat as gone so shutdown proceeds (#50 bounded/force).
		m.logger.Warn("reap: mountpoint check timed out, dropping entry", "path", path)
		return true
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

// unmountKey is the core unmount implementation. It calls m.unmount(ctx, path,
// force) and, on failure, re-checks whether the path is still a mountpoint via
// mountpointGoneCtx. If the path is gone (or the ctx-bounded check times out),
// the entry is reaped — deleted from activeMounts, a WARN is logged, and nil is
// returned. This is the fix for #47 (dead mount reap) and #50 (bounded ctx).
//
// reguard controls whether to re-restrict the target dir to guardMode after the
// entry is removed (#49 guard-target). Pass reguard=true for all interactive and
// device-offline paths (the target stays in config and must be re-restricted so
// stray writes are blocked until the next mount); pass reguard=false only for
// shutdown (UnmountAllForce), where the process is exiting and the chmod is
// pointless. reguard failures are logged at WARN and never returned — a re-guard
// error must not turn a successful unmount into a failure.
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
			// Mount is already gone — reap the stale entry and return success.
			delete(m.activeMounts, key)
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
		// Path is still a mountpoint — real failure; retain entry so a retry is
		// possible.
		return fmt.Errorf("unmount %q (device %q share %q): %w", mnt.LocalPath, key.Device, key.Share, cmdErr)
	}

	// Command succeeded — do NOT re-check (lazy unmount may still look mounted
	// briefly). Delete and log.
	delete(m.activeMounts, key)
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
// reguard is threaded through to unmountKey — see unmountKey for semantics.
func (m *Mounter) unmountAll(ctx context.Context, force, reguard bool) error {
	m.mu.Lock()
	keys := make([]mountKey, 0, len(m.activeMounts))
	for k := range m.activeMounts {
		keys = append(keys, k)
	}
	m.mu.Unlock()

	var errs []string
	for _, key := range keys {
		m.mu.Lock()
		err := m.unmountKey(ctx, key, force, reguard)
		m.mu.Unlock()
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
