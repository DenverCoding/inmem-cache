//go:build linux

package main

// Linux keeps the socket in /run (tmpfs): wipes on reboot, stays under the
// systemd-managed runtime tree, and is the conventional home for ephemeral
// IPC sockets. Root or group-write access on /run is the caller's concern.
func init() {
	DefaultSocketPath = "/run/inmem.sock"
}
