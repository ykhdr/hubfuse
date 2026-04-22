package agent

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	gossh "golang.org/x/crypto/ssh"
)

const (
	privateKeyFile = "id_ed25519"
	publicKeyFile  = "id_ed25519.pub"
)

// GenerateSSHKeyPair generates an ed25519 key pair and saves it to dir.
// The private key is saved to dir/id_ed25519 (mode 0600) and the public key
// to dir/id_ed25519.pub (mode 0644).
// Returns the OpenSSH-format public key string.
func GenerateSSHKeyPair(dir string) (publicKeyStr string, err error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", fmt.Errorf("create key directory %q: %w", dir, err)
	}

	pubKey, privKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", fmt.Errorf("generate ed25519 key: %w", err)
	}

	// Marshal private key in OpenSSH format.
	privBlock, err := gossh.MarshalPrivateKey(privKey, "")
	if err != nil {
		return "", fmt.Errorf("marshal private key: %w", err)
	}
	privPEM := pem.EncodeToMemory(privBlock)

	privPath := filepath.Join(dir, privateKeyFile)
	if err := os.WriteFile(privPath, privPEM, 0600); err != nil {
		return "", fmt.Errorf("write private key %q: %w", privPath, err)
	}

	// Marshal public key in OpenSSH authorised-keys format.
	sshPub, err := gossh.NewPublicKey(pubKey)
	if err != nil {
		return "", fmt.Errorf("create ssh public key: %w", err)
	}
	pubKeyBytes := gossh.MarshalAuthorizedKey(sshPub)
	publicKeyStr = strings.TrimSuffix(string(pubKeyBytes), "\n")

	pubPath := filepath.Join(dir, publicKeyFile)
	if err := os.WriteFile(pubPath, pubKeyBytes, 0644); err != nil {
		return "", fmt.Errorf("write public key %q: %w", pubPath, err)
	}

	return publicKeyStr, nil
}

// LoadPublicKey reads and returns the SSH public key string from path.
func LoadPublicKey(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read public key %q: %w", path, err)
	}
	return strings.TrimSuffix(string(data), "\n"), nil
}

// validateDeviceID rejects device IDs that would be unsafe to use as a
// filename component. Peer IDs arrive from mTLS cert CNs via the hub and are
// not otherwise validated, so every path-forming call site must pass through
// this check to block traversal (e.g. "../etc/passwd") and NUL/control bytes.
func validateDeviceID(deviceID string) error {
	if deviceID == "" {
		return fmt.Errorf("device ID is empty")
	}
	if deviceID == "." || deviceID == ".." {
		return fmt.Errorf("invalid device ID %q", deviceID)
	}
	if strings.ContainsAny(deviceID, `/\`) || strings.Contains(deviceID, "..") {
		return fmt.Errorf("invalid device ID %q: contains path separator or parent reference", deviceID)
	}
	for _, r := range deviceID {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("invalid device ID %q: contains control character", deviceID)
		}
	}
	return nil
}

// SavePeerPublicKey stores the public key for deviceID under knownDevicesDir.
// The file is named <deviceID>.pub and is written with 0644 permissions.
// Parent directories are created as needed.
func SavePeerPublicKey(knownDevicesDir, deviceID, publicKey string) error {
	if err := validateDeviceID(deviceID); err != nil {
		return err
	}

	if err := os.MkdirAll(knownDevicesDir, 0700); err != nil {
		return fmt.Errorf("create known devices directory %q: %w", knownDevicesDir, err)
	}

	path := filepath.Join(knownDevicesDir, deviceID+".pub")
	content := publicKey
	if !strings.HasSuffix(content, "\n") {
		content += "\n"
	}

	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		return fmt.Errorf("write peer public key %q: %w", path, err)
	}

	return nil
}

// LoadPeerPublicKey reads the stored public key for deviceID from knownDevicesDir.
func LoadPeerPublicKey(knownDevicesDir, deviceID string) (string, error) {
	if err := validateDeviceID(deviceID); err != nil {
		return "", err
	}

	path := filepath.Join(knownDevicesDir, deviceID+".pub")
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read peer public key for device %q: %w", deviceID, err)
	}
	return strings.TrimSuffix(string(data), "\n"), nil
}

// ListPairedDevices returns the device IDs of all paired devices whose public
// keys are stored in knownDevicesDir. Each *.pub file contributes one device ID.
func ListPairedDevices(knownDevicesDir string) ([]string, error) {
	entries, err := os.ReadDir(knownDevicesDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read known devices directory %q: %w", knownDevicesDir, err)
	}

	var deviceIDs []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasSuffix(name, ".pub") {
			deviceIDs = append(deviceIDs, strings.TrimSuffix(name, ".pub"))
		}
	}

	return deviceIDs, nil
}
