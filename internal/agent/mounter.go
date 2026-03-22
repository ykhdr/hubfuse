package agent

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	agentconfig "github.com/ykhdr/hubfuse/internal/agent/config"
)

// Mount represents an active SSHFS mount.
type Mount struct {
	Device    string
	Share     string
	LocalPath string
	IP        string
	SSHPort   int
	cmd       *exec.Cmd
}

// Mounter manages SSHFS mounts for remote shares.
type Mounter struct {
	keyPath         string // path to agent's SSH private key
	knownDevicesDir string // path to known_devices directory
	logger          *slog.Logger
	activeMounts    map[string]*Mount // key: "device:share"
	mu              sync.Mutex

	// execCommand is used to build commands; override in tests.
	execCommand func(ctx context.Context, name string, args ...string) *exec.Cmd
	// unmount is used to unmount a path; override in tests.
	unmount func(path string) error
}

// NewMounter creates a new Mounter.
func NewMounter(keyPath, knownDevicesDir string, logger *slog.Logger) *Mounter {
	return &Mounter{
		keyPath:         keyPath,
		knownDevicesDir: knownDevicesDir,
		logger:          logger,
		activeMounts:    make(map[string]*Mount),
		execCommand:     exec.CommandContext,
		unmount:         unmountPath,
	}
}

// mountKey returns the map key for a device+share pair.
func mountKey(device, share string) string {
	return device + ":" + share
}

// Mount mounts the remote share described by mc from deviceIP:sshPort using SSHFS.
// It verifies the peer device is paired (has a key in known_devices) before mounting.
func (m *Mounter) Mount(ctx context.Context, mc agentconfig.MountConfig, deviceIP string, sshPort int) error {
	// Verify peer device is paired.
	pubKeyPath := filepath.Join(m.knownDevicesDir, mc.Device+".pub")
	if _, err := os.Stat(pubKeyPath); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("device %q is not paired: no key found at %q", mc.Device, pubKeyPath)
		}
		return fmt.Errorf("check pairing for device %q: %w", mc.Device, err)
	}

	// Create mount point directory if needed.
	if err := os.MkdirAll(mc.To, 0755); err != nil {
		return fmt.Errorf("create mount point %q: %w", mc.To, err)
	}

	key := mountKey(mc.Device, mc.Share)

	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.activeMounts[key]; exists {
		return fmt.Errorf("share %q from device %q is already mounted", mc.Share, mc.Device)
	}

	// Build sshfs command.
	// The remote path is just the alias; the SSH server maps aliases to real paths.
	cmd := m.execCommand(ctx, "sshfs",
		"-p", fmt.Sprintf("%d", sshPort),
		"-o", fmt.Sprintf("IdentityFile=%s", m.keyPath),
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		fmt.Sprintf("hubfuse@%s:%s", deviceIP, mc.Share),
		mc.To,
	)

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start sshfs for %q from device %q: %w", mc.Share, mc.Device, err)
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

// Unmount unmounts the share identified by device and share name.
// It uses the platform-appropriate unmount command.
func (m *Mounter) Unmount(device, share string) error {
	key := mountKey(device, share)

	m.mu.Lock()
	defer m.mu.Unlock()

	mnt, exists := m.activeMounts[key]
	if !exists {
		return fmt.Errorf("no active mount for device %q share %q", device, share)
	}

	if err := m.unmount(mnt.LocalPath); err != nil {
		return fmt.Errorf("unmount %q (device %q share %q): %w", mnt.LocalPath, device, share, err)
	}

	delete(m.activeMounts, key)

	m.logger.Info("unmounted share",
		"device", device,
		"share", share,
		"local_path", mnt.LocalPath,
	)

	return nil
}

// unmountPath runs the platform-specific unmount command for path.
func unmountPath(path string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("umount", path)
	default:
		// Linux and others.
		cmd = exec.Command("fusermount", "-u", path)
	}

	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// UnmountAll unmounts all active mounts.
// It attempts to unmount each mount and accumulates errors.
func (m *Mounter) UnmountAll() error {
	m.mu.Lock()
	keys := make([]string, 0, len(m.activeMounts))
	for k := range m.activeMounts {
		keys = append(keys, k)
	}
	m.mu.Unlock()

	var errs []string
	for _, key := range keys {
		parts := strings.SplitN(key, ":", 2)
		if len(parts) != 2 {
			continue
		}
		if err := m.Unmount(parts[0], parts[1]); err != nil {
			errs = append(errs, err.Error())
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("unmount errors: %s", strings.Join(errs, "; "))
	}
	return nil
}

// UnmountDevice unmounts all shares from the named device.
func (m *Mounter) UnmountDevice(deviceNickname string) error {
	m.mu.Lock()
	var keys []string
	for k, mnt := range m.activeMounts {
		if mnt.Device == deviceNickname {
			keys = append(keys, k)
		}
	}
	m.mu.Unlock()

	var errs []string
	for _, key := range keys {
		parts := strings.SplitN(key, ":", 2)
		if len(parts) != 2 {
			continue
		}
		if err := m.Unmount(parts[0], parts[1]); err != nil {
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
	_, ok := m.activeMounts[mountKey(device, share)]
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
