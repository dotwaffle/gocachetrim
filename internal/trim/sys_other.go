//go:build !(darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd)

// This file has stubs for systems without flock(2) and st_blocks. On these
// systems, Run fails at the lock step with errors.ErrUnsupported.

package trim

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"runtime"
)

// errWouldBlock is never returned on these systems.
var errWouldBlock = errors.New("lock would block")

// allocated returns the logical size, because st_blocks is not available.
func allocated(fi fs.FileInfo) int64 { return fi.Size() }

// tryLock always fails with errors.ErrUnsupported.
func tryLock(*os.File) error {
	return fmt.Errorf("file locks on %s: %w", runtime.GOOS, errors.ErrUnsupported)
}
