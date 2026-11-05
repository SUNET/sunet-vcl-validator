package main

import (
	"fmt"
	"math"
	"os"
	"syscall"
)

// socketsSupported tells whether mknodSocketAt can create sockets on this OS.
const socketsSupported = true

// mknodSocketAt creates a socket file entry called name in the directory dir.
func mknodSocketAt(dir *os.File, name string) error {
	// Fd() returns a uintptr while Mknodat wants an int. A closed or nil
	// file also ends up here since Fd() then returns ^uintptr(0).
	rawFd := dir.Fd()
	if rawFd > math.MaxInt {
		return fmt.Errorf("invalid socket directory file descriptor %d", rawFd)
	}
	return syscall.Mknodat(int(rawFd), name, syscall.S_IFSOCK|0o600, 0)
}
