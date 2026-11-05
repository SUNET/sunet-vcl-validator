//go:build !linux

package main

import (
	"errors"
	"fmt"
	"os"
)

// Go only has a mknodat(2) wrapper on Linux. The service only runs in a Linux
// container, this is so it can still be built, vetted and mostly tested on
// other systems such as macOS.

// socketsSupported tells whether mknodSocketAt can create sockets on this OS.
const socketsSupported = false

func mknodSocketAt(_ *os.File, _ string) error {
	return fmt.Errorf("creating dummy sockets is only supported on Linux: %w", errors.ErrUnsupported)
}
