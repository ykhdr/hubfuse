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

// share records a directory export: the real filesystem path and the alias
// clients use to reference it.
type share struct {
	path  string
	alias string
}

// Agent wraps invocation of the `hubfuse` binary against a hub, with an
// isolated HOME directory so agents do not touch each other's state.
type Agent struct {
	Nickname     string
	HomeDir      string
	SSHPort      int
	StubMountDir string

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

// WithExport appends a directory export with the given alias to the agent.
// The path is created during StartDaemon; alias is the name clients use.
func WithExport(path, alias string) AgentOption {
	return func(a *Agent) {
		a.exports = append(a.exports, share{path: path, alias: alias})
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
	a.logBuf.Write([]byte("$ hubfuse " + strings.Join(args, " ") + "\n"))
	a.logBuf.Write(out)
	if err != nil {
		t.Fatalf("hubfuse %s failed: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// runExpectFail executes `hubfuse <args...>` and returns combined output; it
// does NOT fail the test on non-zero exit. Useful for asserting error paths.
func (a *Agent) runExpectFail(t *testing.T, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, AgentBinaryPath, args...)
	cmd.Env = a.env()
	out, _ := cmd.CombinedOutput()
	a.logBuf.Write([]byte("$ hubfuse " + strings.Join(args, " ") + "  (expecting failure)\n"))
	a.logBuf.Write(out)
	return string(out)
}

// Join runs `hubfuse join <hub-addr>` with the nickname fed via stdin (the
// command prompts "Enter nickname for this device: ").
func (a *Agent) Join(t *testing.T) {
	t.Helper()
	stdin := []byte(a.Nickname + "\n")
	a.runWithStdin(t, stdin, "join", a.hub.Address)
}

// StartDaemon launches the hubfuse daemon with the agent's configuration.
// It updates config.kdl with the SSH port and exports, then starts the daemon.
// Returns once the SSH server port is confirmed listening.
func (a *Agent) StartDaemon(t *testing.T) {
	t.Helper()

	// Pick a free port if not already set.
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
		cfg.Hub.Address = a.hub.Address
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
	daemonEnv = append(daemonEnv, a.envExtra...)

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

	// Add exports via CLI AFTER the daemon is running so the config-file watcher
	// fires and the SSH server's alias→path map is updated. Writing shares to
	// config.kdl before start does NOT populate the SSH server because the
	// watcher's onChange callback is only triggered by file-change events, not
	// by the initial file state at startup.
	for _, s := range a.exports {
		if mkErr := os.MkdirAll(s.path, 0o755); mkErr != nil {
			t.Fatalf("mkdir export %s: %v", s.path, mkErr)
		}
		a.run(t, "share", "add", s.path, "--alias", s.alias)
	}
}

// Stop signals the daemon to exit and waits up to 5s for it to do so.
// Idempotent — safe to call multiple times.
func (a *Agent) Stop(t *testing.T) {
	t.Helper()
	if a.daemonCmd == nil || a.daemonCmd.Process == nil {
		return
	}
	_ = a.daemonCmd.Process.Signal(syscall.SIGTERM)
	done := make(chan error, 1)
	go func() { done <- a.daemonCmd.Wait() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		_ = a.daemonCmd.Process.Kill()
		<-done
	}
	if a.daemonCancel != nil {
		a.daemonCancel()
	}
	a.daemonCmd = nil
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

// ConfirmPairing completes a pairing handshake using the given invite code.
// It calls ConfirmPairing directly via gRPC (there is no CLI for this side).
// After the RPC completes, it saves the peer's public key to
// known_devices/<peerDeviceID>.pub so the local daemon can authenticate the
// peer's SSH connections without waiting for a PairingCompleted event (which
// the hub only sends to the initiator, not the confirmer).
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

	peerPublicKey, err := client.ConfirmPairing(ctx, inviteCode, myPubKey)
	if err != nil {
		t.Fatalf("ConfirmPairing: RPC: %v", err)
	}

	// The hub sends PairingCompleted only to the initiator (alice), not to the
	// confirmer (bob). Save alice's public key manually so the local daemon's
	// isPaired check passes when bob tries to mount alice's share.
	if peerPublicKey != "" {
		devResp, listErr := client.ListDevices(ctx)
		if listErr != nil {
			t.Logf("ConfirmPairing: ListDevices failed (non-fatal): %v", listErr)
		} else {
			// Find the device whose public key matches peerPublicKey by nickname
			// exclusion — in a two-device test this is always the other device.
			knownDevicesDir := filepath.Join(hubDir, common.KnownDevicesDir)
			for _, dev := range devResp.Devices {
				if dev.Nickname != a.Nickname {
					if saveErr := agent.SavePeerPublicKey(knownDevicesDir, dev.DeviceId, peerPublicKey); saveErr != nil {
						t.Logf("ConfirmPairing: save peer key (non-fatal): %v", saveErr)
					}
					break
				}
			}
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
	knownDir := filepath.Join(a.HomeDir, ".hubfuse", common.KnownDevicesDir)
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		entries, err := os.ReadDir(knownDir)
		if err == nil && len(entries) > 0 {
			// Sleep briefly to allow reloadSSHAllowedKeys to finish loading the
			// key into the SSH server's in-memory cache. The file write and the
			// cache reload happen sequentially in the daemon's event goroutine,
			// but the test goroutine can observe the file before the cache is
			// updated.
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

// sanitizeForMarker mirrors the stub's sanitize function to compute the JSON
// marker filename for a given mount destination path.
func sanitizeForMarker(p string) string {
	r := strings.NewReplacer("/", "_", `\`, "_", ":", "_", " ", "_")
	return r.Replace(strings.TrimPrefix(p, "/"))
}

