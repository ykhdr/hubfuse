package helpers

import "os"

// existingPath returns the test process's PATH so subprocess tests can find
// any helper binaries already on it (e.g. stub-sshfs in Task 6).
func existingPath() string { return os.Getenv("PATH") }
