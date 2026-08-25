package agent

import (
	"errors"
	"syscall"
)

// errnoIsHostUnreachable reports whether err wraps EHOSTUNREACH. Split into its
// own file only to keep the syscall import away from the pure-string logic in
// localnetwork.go; the constant exists on every platform this project builds
// for. (#74)
func errnoIsHostUnreachable(err error) bool {
	return errors.Is(err, syscall.EHOSTUNREACH)
}
