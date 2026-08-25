package helpers

import (
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ykhdr/hubfuse/internal/agent"
	"github.com/ykhdr/hubfuse/internal/agent/config"
	"github.com/ykhdr/hubfuse/internal/common"
)

// share records a directory export: real filesystem path, alias clients use,
// and the ACL (--permissions and --allow) passed to `hubfuse share add`.
type share struct {
	path        string
	alias       string
	permissions string   // "" = use CLI default ("ro"); "rw" or "ro"
	allow       []string // tokens for --allow (nicknames, "all", or device_ids)
}

// Agent wraps invocation of the `hubfuse` binary against a hub, with an
// isolated HOME directory so agents do not touch each other's state.
type Agent struct {
	Nickname     string
	HomeDir      string
	SSHPort      int
	StubMountDir string

	// hubAddr is the address this agent joins and connects to. It defaults to
	// the hub's own address; WithHubAddress points it somewhere else — a relay
	// standing in for a network that can stop delivering (#72).
	hubAddr string

	hub          *Hub
	logBuf       *LogBuffer
	envExtra     []string
	exports      []share
	daemonCmd    *exec.Cmd
	daemonCancel context.CancelFunc
}

type AgentOption func(*Agent)

func WithEnv(kv ...string) AgentOption {
	return func(a *Agent) { a.envExtra = append(a.envExtra, kv...) }
}

// WithHubAddress makes the agent join and connect through addr instead of the
// hub's own address. Used to put a testrelay.Relay in front of the hub so a
// scenario can silence the connection without either side being told. (#72)
func WithHubAddress(addr string) AgentOption {
	return func(a *Agent) { a.hubAddr = addr }
}

// WithExport appends a directory export with the given alias to the agent.
// The path is created during StartDaemon; alias is the name clients use.
// Defaults to --allow all so scenarios that don't care about ACL behaviour
// keep working under the secure-default semantics enforced by the SSH server.
func WithExport(path, alias string) AgentOption {
	return func(a *Agent) {
		a.exports = append(a.exports, share{path: path, alias: alias, allow: []string{"all"}})
	}
}

// WithExportACL appends a directory export with explicit permissions and
// allowed-devices. Use this in tests that exercise ACL behaviour.
// permissions: "ro" | "rw" | "" (CLI default "ro").
// allow: tokens for --allow; pass no tokens to omit --allow entirely (i.e. test
// the default-deny path).
func WithExportACL(path, alias, permissions string, allow ...string) AgentOption {
	return func(a *Agent) {
		a.exports = append(a.exports, share{
			path:        path,
			alias:       alias,
			permissions: permissions,
			allow:       append([]string(nil), allow...),
		})
	}
}

// WithSSHPort overrides the default free-port selection for the agent's SSH server.
func WithSSHPort(port int) AgentOption {
	return func(a *Agent) { a.SSHPort = port }
}

// StartAgent prepares an isolated HOME for the agent. It does NOT launch a
// daemon process — use Join / run / runExpectFail for one-shot invocations.
func StartAgent(t *testing.T, hub *Hub, nickname string, opts ...AgentOption) *Agent {
	t.Helper()
	home := t.TempDir()
	a := &Agent{
		Nickname: nickname,
		HomeDir:  home,
		hubAddr:  hub.Address,
		hub:      hub,
		logBuf:   &LogBuffer{},
	}
	for _, o := range opts {
		o(a)
	}
	DumpOnFailure(t, "agent:"+nickname, a.logBuf)
	return a
}

// run executes `hubfuse <args...>` with the agent's HOME and returns combined
// output. Test fails on non-zero exit.
func (a *Agent) run(t *testing.T, args ...string) string {
	t.Helper()
	return a.runWithStdin(t, nil, args...)
}

// runWithStdin variant that can pipe bytes to the child process stdin.
func (a *Agent) runWithStdin(t *testing.T, stdin []byte, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, AgentBinaryPath, args...)
	cmd.Env = a.env()
	if stdin != nil {
		cmd.Stdin = strings.NewReader(string(stdin))
	}
	out, err := cmd.CombinedOutput()
	_, _ = a.logBuf.Write([]byte("$ hubfuse " + strings.Join(args, " ") + "\n"))
	_, _ = a.logBuf.Write(out)
	if err != nil {
		t.Fatalf("hubfuse %s failed: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// tryRun executes the hubfuse binary and returns (output, true) on zero-exit
// or (output, false) on non-zero. It never fails the test; useful for polling
// loops where a transient failure (e.g. hub restarting) is expected.
func (a *Agent) tryRun(t *testing.T, args ...string) (string, bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, AgentBinaryPath, args...)
	cmd.Env = a.env()
	out, err := cmd.CombinedOutput()
	_, _ = a.logBuf.Write([]byte("$ hubfuse " + strings.Join(args, " ") + "  (try)\n"))
	_, _ = a.logBuf.Write(out)
	return string(out), err == nil
}

// Join runs `hubfuse join <hub-addr> --token <token>` with the nickname fed
// via stdin. It automatically issues a one-time join token from the hub so
// callers do not need to manage the token lifecycle.
func (a *Agent) Join(t *testing.T) {
	t.Helper()
	token := a.hub.IssueJoinToken(t)
	stdin := []byte(a.Nickname + "\n")
	a.runWithStdin(t, stdin, "join", a.hubAddr, "--token", token)
}

// TryJoinWithoutToken runs `hubfuse join <addr>` with no --token flag and
// returns the combined output and whether the command exited zero. It never
// fails the test — use it to assert the negative (non-zero exit) path.
func (a *Agent) TryJoinWithoutToken(t *testing.T, hubAddr string) (string, bool) {
	t.Helper()
	return a.tryRun(t, "join", hubAddr)
}

// TryJoinWithTamperedToken runs `hubfuse join <addr> --token <token>` with the
// given nickname on stdin. It returns the combined output and whether the
// command exited zero. It never fails the test — use it to assert the negative
// (non-zero exit) path when testing fingerprint validation.
func (a *Agent) TryJoinWithTamperedToken(t *testing.T, hubAddr, token, nickname string) (string, bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, AgentBinaryPath, "join", hubAddr, "--token", token)
	cmd.Env = a.env()
	cmd.Stdin = strings.NewReader(nickname + "\n")
	out, err := cmd.CombinedOutput()
	_, _ = a.logBuf.Write([]byte("$ hubfuse join " + hubAddr + " --token <tampered>  (try)\n"))
	_, _ = a.logBuf.Write(out)
	return string(out), err == nil
}

// TryRun runs the hubfuse binary with arbitrary args and returns (output, ok).
// Use this for negative-path assertions where the test cares whether the
// command exited non-zero. Never fails the test by itself.
func (a *Agent) TryRun(t *testing.T, args ...string) (string, bool) {
	t.Helper()
	return a.tryRun(t, args...)
}

// StartDaemon launches the hubfuse daemon with the agent's configuration.
// It updates config.kdl with the SSH port and exports, then starts the daemon.
// Returns once the SSH server port is confirmed listening.
func (a *Agent) StartDaemon(t *testing.T) {
	t.Helper()

	a.launchDaemon(t)

	// Add exports via CLI AFTER the daemon is running so the config-file watcher
	// fires and the SSH server's alias→path map is updated. Writing shares to
	// config.kdl before start does NOT populate the SSH server because the
	// watcher's onChange callback is only triggered by file-change events, not
	// by the initial file state at startup.
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
}

// RestartDaemon relaunches the daemon after Stop, reusing the SSH port picked
// on first start and deliberately NOT re-running `share add`: the exports were
// persisted into config.kdl by the first StartDaemon (share add writes them
// there), and NewDaemon installs the initial ACL snapshot from the loaded
// config, so a restarted daemon serves its shares immediately without any
// config-watcher event. Calling StartDaemon a second time would instead re-run
// `share add` and duplicate the share entries in config.kdl.
func (a *Agent) RestartDaemon(t *testing.T) {
	t.Helper()
	a.launchDaemon(t)
}

// prepareDaemonRun performs the setup every daemon launch needs — SSH port
// selection, the stub-marker directory, and the config.kdl rewrite — and
// returns the environment for the daemon process. It is shared by launchDaemon
// and StartDaemonExpectFailure so a daemon expected to die runs under exactly
// the same conditions as one expected to live. It does NOT touch exports.
func (a *Agent) prepareDaemonRun(t *testing.T) []string {
	t.Helper()

	// Pick a free port if not already set (RestartDaemon keeps the first one).
	if a.SSHPort == 0 {
		a.SSHPort = FreePort(t)
	}

	// Set up the stub mount marker directory.
	a.StubMountDir = filepath.Join(a.HomeDir, "stub-marker")
	if err := os.MkdirAll(a.StubMountDir, 0o755); err != nil {
		t.Fatalf("mkdir stub-marker: %v", err)
	}

	// Load existing config (written by Join), or start from defaults.
	cfgPath := filepath.Join(a.HomeDir, ".hubfuse", common.ConfigFile)
	cfg, err := config.Load(cfgPath)
	if err != nil {
		// Join should have written this, but be defensive.
		cfg = config.DefaultConfig()
		cfg.Device.Nickname = a.Nickname
		cfg.Hub.Address = a.hubAddr
	}

	// Apply SSH port override — write only the port; shares are added after
	// the daemon starts (via hubfuse share add) so the config-watcher fires and
	// the SSH server's alias→path map is populated.
	cfg.Agent.SSHPort = a.SSHPort

	if err := config.Save(cfgPath, cfg); err != nil {
		t.Fatalf("save config: %v", err)
	}

	// Build the daemon environment with the stub-sshfs directory prepended to
	// PATH. We construct it directly (not via a.env()) to avoid duplicate PATH
	// entries — on macOS, the first occurrence of a key wins, so appending a
	// second PATH=... to the slice returned by a.env() would leave the stub
	// directory silently ignored.
	stubDir := filepath.Dir(StubSSHFSBinaryPath)
	daemonEnv := []string{
		"HOME=" + a.HomeDir,
		"PATH=" + stubDir + ":" + existingPath(),
		"HUBFUSE_STUB_MOUNT_DIR=" + a.StubMountDir,
	}
	return append(daemonEnv, a.envExtra...)
}

// StartDaemonExpectFailure runs `hubfuse start` in the foreground and waits for
// it to exit non-zero, returning the combined output. Use it for the startup
// paths that must abort loudly — the pruned identity of issue #69, where the
// hub refuses the registration and the daemon has nothing useful left to do.
//
// It deliberately does not go through launchDaemon: that one waits for the SSH
// port and registers a Stop cleanup, both meaningless for a process that is
// supposed to die on its own.
func (a *Agent) StartDaemonExpectFailure(t *testing.T, timeout time.Duration) string {
	t.Helper()

	daemonEnv := a.prepareDaemonRun(t)

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, AgentBinaryPath, "start")
	cmd.Env = daemonEnv
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	out, err := cmd.CombinedOutput()
	_, _ = a.logBuf.Write([]byte("$ hubfuse start  (expect failure)\n"))
	_, _ = a.logBuf.Write(out)

	if ctx.Err() != nil {
		t.Fatalf("StartDaemonExpectFailure: %s's daemon was still running after %s; output:\n%s",
			a.Nickname, timeout, out)
	}
	if err == nil {
		t.Fatalf("StartDaemonExpectFailure: %s's daemon exited zero, expected a startup failure; output:\n%s",
			a.Nickname, out)
	}
	return string(out)
}

// AddMount registers a mount in config.kdl WITHOUT waiting for it to come up.
// Unlike Mount, it makes no claim that the mount can succeed — which is the
// point for scenarios that need a configured-but-unreachable mount (issue #69:
// a stale entry that burns the whole mount-verify window on every attempt).
// The daemon may be stopped when this is called; the entry is then picked up by
// the next start.
func (a *Agent) AddMount(t *testing.T, src, dst string) {
	t.Helper()
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatalf("AddMount: mkdir %s: %v", dst, err)
	}
	a.run(t, "mount", "add", src, "--to", dst)
}

// launchDaemon is the daemon-process core shared by StartDaemon and
// RestartDaemon: it prepares the run (see prepareDaemonRun), starts
// `hubfuse start` with the stub-sshfs PATH override, and returns once the SSH
// server port is confirmed listening. It does NOT touch exports.
func (a *Agent) launchDaemon(t *testing.T) {
	t.Helper()

	daemonEnv := a.prepareDaemonRun(t)

	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, AgentBinaryPath, "start")
	cmd.Env = daemonEnv
	cmd.Stdout = a.logBuf
	cmd.Stderr = a.logBuf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatalf("start daemon %s: %v", a.Nickname, err)
	}

	a.daemonCmd = cmd
	a.daemonCancel = cancel

	t.Cleanup(func() { a.Stop(t) })

	// Wait until the SSH server is accepting connections.
	WaitForPort(t, a.SSHPort, 5*time.Second)
}

// Stop signals the daemon to exit and waits up to 5s for it to do so.
// Idempotent — safe to call multiple times.
//
// The daemon is launched with Setpgid=true so its mounter children (including
// stub-sshfs, which blocks on SIGTERM and is not a real FUSE mount) live in
// the daemon's process group. We signal the whole group so children do not
// leak across tests — important for -count=N and for the prune/reconnect
// scenarios that spawn multiple daemons in sequence.
func (a *Agent) Stop(t *testing.T) {
	t.Helper()
	if a.daemonCmd == nil || a.daemonCmd.Process == nil {
		return
	}

	pid := a.daemonCmd.Process.Pid
	pgid, pgidErr := syscall.Getpgid(pid)
	if pgidErr == nil {
		_ = syscall.Kill(-pgid, syscall.SIGTERM)
	} else {
		_ = a.daemonCmd.Process.Signal(syscall.SIGTERM)
	}

	done := make(chan error, 1)
	go func() { done <- a.daemonCmd.Wait() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		if pgidErr == nil {
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
		} else {
			_ = a.daemonCmd.Process.Kill()
		}
		<-done
	}
	if a.daemonCancel != nil {
		a.daemonCancel()
	}
	a.daemonCmd = nil
}

// Leave runs `hubfuse leave` for this agent and fatals if it exits non-zero.
func (a *Agent) Leave(t *testing.T) {
	t.Helper()
	a.run(t, "leave")
}

// TryLeave runs `hubfuse leave` and returns (output, true) on success or
// (output, false) on non-zero exit. Never fails the test.
func (a *Agent) TryLeave(t *testing.T) (string, bool) {
	t.Helper()
	return a.tryRun(t, "leave")
}

// RequestPairing runs `hubfuse pair <targetNickname>` and returns the invite
// code printed by the command. Fatals if the expected line is not found.
func (a *Agent) RequestPairing(t *testing.T, targetNickname string) string {
	t.Helper()
	out := a.run(t, "pair", targetNickname)
	const prefix = "pairing invite code: "
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, prefix) {
			return strings.TrimSpace(strings.TrimPrefix(line, prefix))
		}
	}
	t.Fatalf("RequestPairing: did not find %q in output:\n%s", prefix, out)
	return ""
}

// ConfirmPairingCLI runs `hubfuse pair-confirm <inviteCode>` and returns the
// combined output. Fatals if the command exits non-zero.
func (a *Agent) ConfirmPairingCLI(t *testing.T, inviteCode string) string {
	t.Helper()
	return a.run(t, "pair-confirm", inviteCode)
}

// ConfirmPairing completes a pairing handshake using the given invite code.
// It calls ConfirmPairing directly via gRPC (the `pair-confirm` CLI exists,
// but tests that don't run a daemon prefer the direct path). After the RPC,
// it saves the peer's public key to known_devices/<peerDeviceID>.pub as a
// daemon-offline fallback — when a daemon IS running it also receives a
// PairingCompleted event from the hub and writes the same file idempotently.
func (a *Agent) ConfirmPairing(t *testing.T, inviteCode string) {
	t.Helper()

	hubDir := filepath.Join(a.HomeDir, ".hubfuse")
	tlsDir := filepath.Join(hubDir, common.TLSDir)
	caPath := filepath.Join(tlsDir, common.CACertFile)
	certPath := filepath.Join(tlsDir, common.ClientCertFile)
	keyPath := filepath.Join(tlsDir, common.ClientKeyFile)

	pubKeyPath := filepath.Join(hubDir, common.KeysDir, common.PublicKeyFile)
	pubKeyBytes, err := os.ReadFile(pubKeyPath)
	if err != nil {
		t.Fatalf("ConfirmPairing: read pubkey %s: %v", pubKeyPath, err)
	}
	myPubKey := strings.TrimSpace(string(pubKeyBytes))

	logger := slog.New(common.NewConsoleHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelInfo}))

	client, err := agent.DialWithMTLS(a.hub.Address, caPath, certPath, keyPath, logger)
	if err != nil {
		t.Fatalf("ConfirmPairing: dial hub: %v", err)
	}
	defer func() { _ = client.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	peerPublicKey, peerDeviceID, _, err := client.ConfirmPairing(ctx, inviteCode, myPubKey)
	if err != nil {
		t.Fatalf("ConfirmPairing: RPC: %v", err)
	}

	// Save the peer key locally regardless of whether a daemon is running:
	// when one is, the hub-emitted PairingCompleted event will write the same
	// file too; when one isn't (most scenarios), this is the only writer and
	// is what makes isPaired return true on the next daemon start.
	if peerPublicKey != "" && peerDeviceID != "" {
		knownDevicesDir := filepath.Join(hubDir, common.KnownDevicesDir)
		if saveErr := agent.SavePeerPublicKey(knownDevicesDir, peerDeviceID, peerPublicKey); saveErr != nil {
			t.Logf("ConfirmPairing: save peer key (non-fatal): %v", saveErr)
		}
	}
}

// Mount runs `hubfuse mount add <src> --to <dst>`, creates the destination
// directory, and polls until the stub-sshfs marker file appears.
func (a *Agent) Mount(t *testing.T, src, dst string) {
	t.Helper()
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatalf("Mount: mkdir %s: %v", dst, err)
	}
	a.run(t, "mount", "add", src, "--to", dst)

	markerPath := a.MountMarker(dst)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(markerPath); err == nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}

	// Timeout — dump stub-marker dir for diagnostics.
	entries, _ := os.ReadDir(a.StubMountDir)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	t.Fatalf("Mount: marker %s never appeared after 10s; stub-marker dir contains: %v", markerPath, names)
}

// MountMarker returns the path of the stub-sshfs JSON marker for the given
// mount destination. The marker exists only while the stub process is running.
func (a *Agent) MountMarker(dst string) string {
	return filepath.Join(a.StubMountDir, sanitizeForMarker(dst)+".json")
}

// WaitForDaemonLog polls this agent's captured daemon stdout/stderr until it
// contains substr, failing the test after timeout. Use it to sequence a
// scenario on the daemon's own progress rather than on external side effects.
// The canonical example: after Mount(), waiting for "mounted share" proves the
// daemon's verify-poll completed and the mount is recorded in activeMounts —
// the stub marker alone appears up to one verify poll-interval EARLIER, and
// killing the stub inside that window aborts the still-in-flight mount (no
// activeMounts entry, nothing to heal) instead of creating a dead mount.
// DaemonLog returns everything the daemon has written to stdout/stderr so far.
func (a *Agent) DaemonLog() string { return a.logBuf.String() }

func (a *Agent) WaitForDaemonLog(t *testing.T, substr string, timeout time.Duration) {
	t.Helper()
	a.WaitForDaemonLogCount(t, substr, 1, timeout)
}

// WaitForDaemonLogCount is WaitForDaemonLog for a line that must appear a given
// number of times — the way a scenario observes a REPEATED transition, e.g. a
// second "registered with hub" proving the daemon re-established its session
// rather than merely still holding the first one. (#72)
func (a *Agent) WaitForDaemonLogCount(t *testing.T, substr string, want int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if strings.Count(a.logBuf.String(), substr) >= want {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("WaitForDaemonLogCount: %q appeared %d times (want %d) in %s's daemon log within %s",
		substr, strings.Count(a.logBuf.String(), substr), want, a.Nickname, timeout)
}

// KillStubMount simulates "the sshfs process died" (issue #67) for this
// agent's mount at dst: it SIGTERMs the stub-sshfs process recorded in the
// mount's marker and waits for the marker to disappear (the stub's defer
// removes it on the way out). Strictly SIGTERM, never SIGKILL — a SIGKILLed
// stub skips its defer and strands the marker, and the agent-side liveness
// probe then reads the unreaped zombie as ALIVE (see the zombie caveat in
// internal/agent/stubmount.go), which would defeat the very healing this
// helper exists to provoke.
//
// No hub event accompanies the kill: the peer stays registered at an
// unchanged endpoint, so this reproduces exactly the event-less dead-mount
// window that only the mount monitor can cover.
func (a *Agent) KillStubMount(t *testing.T, dst string) {
	t.Helper()
	markerPath := a.MountMarker(dst)
	marker := ReadMarker(t, markerPath)
	if marker.PID <= 0 {
		t.Fatalf("KillStubMount: marker %s has no usable pid: %+v", markerPath, marker)
	}
	if err := syscall.Kill(marker.PID, syscall.SIGTERM); err != nil {
		t.Fatalf("KillStubMount: SIGTERM stub pid %d: %v", marker.PID, err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(markerPath); os.IsNotExist(err) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("KillStubMount: marker %s still present 5s after SIGTERM to stub pid %d", markerPath, marker.PID)
}

// HasPeer returns true if `hubfuse devices` lists the given nickname.
func (a *Agent) HasPeer(t *testing.T, nickname string) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, AgentBinaryPath, "devices")
	cmd.Env = a.env()
	out, _ := cmd.CombinedOutput()
	return strings.Contains(string(out), nickname)
}

// WaitForPairedWith polls until this agent's known_devices directory has at
// least one entry, indicating the daemon processed a PairingCompleted event and
// saved a peer's public key. Returns true on success, false on timeout.
//
// This must be called after ConfirmPairing on the initiating side: the hub
// sends PairingCompleted asynchronously via the subscribe stream, so there is a
// brief window where the SSH server has not yet loaded the peer key.
//
// After the peer key file appears, a short stabilisation sleep is applied so
// that reloadSSHAllowedKeys (called immediately after SavePeerPublicKey in the
// daemon's event handler) has time to complete before the caller proceeds to
// mount.
func (a *Agent) WaitForPairedWith(t *testing.T, timeout time.Duration) bool {
	t.Helper()
	return a.WaitForPairedCount(t, 1, timeout)
}

// WaitForPairedCount polls until this agent's known_devices directory has at
// least n entries, then pauses briefly so the SSH server's allowed-key cache
// can catch up with the on-disk state. Used by multi-peer scenarios.
func (a *Agent) WaitForPairedCount(t *testing.T, n int, timeout time.Duration) bool {
	t.Helper()
	knownDir := filepath.Join(a.HomeDir, ".hubfuse", common.KnownDevicesDir)
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		entries, err := os.ReadDir(knownDir)
		if err == nil && len(entries) >= n {
			time.Sleep(200 * time.Millisecond)
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}

// env builds the environment for a subprocess invocation of hubfuse. HOME is
// the agent's isolated dir; PATH is inherited from the test process.
func (a *Agent) env() []string {
	base := []string{
		"HOME=" + a.HomeDir,
		"PATH=" + existingPath(),
	}
	return append(base, a.envExtra...)
}

// sanitizeForMarker computes the JSON marker filename stem for a given mount
// destination path.
//
// KEEP IN SYNC — this transformation exists in THREE packages that cannot
// import each other (a main package, this test helper package, and the agent):
//   - sanitize() in tests/tools/stub-sshfs/main.go (marker writer)
//   - sanitizeForMarker() here (test-side reader)
//   - stubSanitizePath() in internal/agent/stubmount.go (agent-side liveness
//     check + stub unmount, issue #67)
//
// Any drift makes the agent look for markers at the wrong path: every mount
// verify-poll would time out and every liveness probe would read "dead".
func sanitizeForMarker(p string) string {
	r := strings.NewReplacer("/", "_", `\`, "_", ":", "_", " ", "_")
	return r.Replace(strings.TrimPrefix(p, "/"))
}
