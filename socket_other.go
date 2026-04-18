//go:build !linux

package main

import (
	"os"
	"path/filepath"
)

// macOS, Windows, and other non-Linux targets do not have a user-writable
// /run, so the default falls back to the OS temp directory. Windows 10+
// supports AF_UNIX natively, so the listen path works as-is; access is
// gated by NTFS ACLs on the parent directory rather than socket file perms.
func init() {
	DefaultSocketPath = filepath.Join(os.TempDir(), "inmem.sock")
}
