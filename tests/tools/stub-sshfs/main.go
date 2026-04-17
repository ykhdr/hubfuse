// stub-sshfs is a drop-in for real sshfs used by HubFuse scenario tests. It
// parses the same CLI surface, performs a real SSH+SFTP handshake against the
// target, writes a JSON marker describing the mount, and blocks on SIGTERM.
// It intentionally does NOT create a FUSE mount.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// Argument shape (matching real sshfs CLI):
//
//	stub-sshfs user@host:/remote/path /local/mount-point -p PORT -o IdentityFile=PATH [-o ...]

type Marker struct {
	Src         string   `json:"src"`
	Dst         string   `json:"dst"`
	RemoteUser  string   `json:"remote_user"`
	RemoteHost  string   `json:"remote_host"`
	RemotePort  int      `json:"remote_port"`
	RemotePath  string   `json:"remote_path"`
	KeyPath     string   `json:"key_path"`
	RemoteFiles []string `json:"remote_files"`
	PID         int      `json:"pid"`
}

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: stub-sshfs user@host:/path /mount-point [-p PORT] [-o KEY=VAL ...]")
		os.Exit(2)
	}
	src := os.Args[1]
	dst := os.Args[2]

	fs := flag.NewFlagSet("stub-sshfs", flag.ContinueOnError)
	port := fs.Int("p", 22, "ssh port")
	var opts arrayFlag
	fs.Var(&opts, "o", "ssh option (KEY=VAL)")
	if err := fs.Parse(os.Args[3:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}

	user, host, remotePath := parseSrc(src)
	if user == "" || host == "" {
		fmt.Fprintf(os.Stderr, "stub-sshfs: cannot parse src %q\n", src)
		os.Exit(2)
	}

	keyPath := optValue(opts, "IdentityFile")
	if keyPath == "" {
		fmt.Fprintln(os.Stderr, "stub-sshfs: -o IdentityFile=... is required")
		os.Exit(2)
	}

	keyBytes, err := os.ReadFile(keyPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "stub-sshfs: read key %s: %v\n", keyPath, err)
		os.Exit(2)
	}
	signer, err := ssh.ParsePrivateKey(keyBytes)
	if err != nil {
		fmt.Fprintf(os.Stderr, "stub-sshfs: parse key: %v\n", err)
		os.Exit(2)
	}

	cfg := &ssh.ClientConfig{
		User:            user,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         5 * time.Second,
	}
	addr := fmt.Sprintf("%s:%d", host, *port)
	client, err := ssh.Dial("tcp", addr, cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "stub-sshfs: ssh dial %s: %v\n", addr, err)
		os.Exit(1)
	}
	defer func() { _ = client.Close() }()

	sftpClient, err := sftp.NewClient(client)
	if err != nil {
		fmt.Fprintf(os.Stderr, "stub-sshfs: sftp: %v\n", err)
		os.Exit(1)
	}
	defer func() { _ = sftpClient.Close() }()

	entries, err := sftpClient.ReadDir(remotePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "stub-sshfs: readdir %s: %v\n", remotePath, err)
		os.Exit(1)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}

	markerDir := os.Getenv("HUBFUSE_STUB_MOUNT_DIR")
	if markerDir == "" {
		markerDir = os.TempDir()
	}
	if err := os.MkdirAll(markerDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "stub-sshfs: mkdir markerDir: %v\n", err)
		os.Exit(1)
	}
	markerPath := filepath.Join(markerDir, sanitize(dst)+".json")

	marker := Marker{
		Src:         src,
		Dst:         dst,
		RemoteUser:  user,
		RemoteHost:  host,
		RemotePort:  *port,
		RemotePath:  remotePath,
		KeyPath:     keyPath,
		RemoteFiles: names,
		PID:         os.Getpid(),
	}
	if err := writeJSON(markerPath, &marker); err != nil {
		fmt.Fprintf(os.Stderr, "stub-sshfs: write marker: %v\n", err)
		os.Exit(1)
	}
	defer func() { _ = os.Remove(markerPath) }()

	// Block on SIGTERM/SIGINT to mimic real sshfs daemonize behavior.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	<-sig
}

func parseSrc(s string) (user, host, path string) {
	at := strings.IndexByte(s, '@')
	if at < 0 {
		return "", "", ""
	}
	user = s[:at]
	rest := s[at+1:]
	colon := strings.IndexByte(rest, ':')
	if colon < 0 {
		return user, rest, ""
	}
	host = rest[:colon]
	path = rest[colon+1:]
	return
}

type arrayFlag []string

func (a *arrayFlag) String() string     { return strings.Join(*a, ",") }
func (a *arrayFlag) Set(v string) error { *a = append(*a, v); return nil }

func optValue(opts []string, key string) string {
	for _, o := range opts {
		if eq := strings.IndexByte(o, '='); eq >= 0 && o[:eq] == key {
			return o[eq+1:]
		}
	}
	return ""
}

func sanitize(p string) string {
	r := strings.NewReplacer("/", "_", `\`, "_", ":", "_", " ", "_")
	return r.Replace(strings.TrimPrefix(p, "/"))
}

func writeJSON(path string, v any) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
