//go:build darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd

package trim

import (
	"errors"
	"io/fs"
	"os"
	"syscall"
)

// errWouldBlock is the error of tryLock when a different process holds the
// lock.
var errWouldBlock = syscall.EWOULDBLOCK

// allocated returns the disk space of a file. If fi has no Stat_t, it
// returns the logical size.
func allocated(fi fs.FileInfo) int64 {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return st.Blocks * 512
	}
	return fi.Size()
}

// tryLock takes an exclusive flock(2) lock on f without blocking, the same
// lock type that the go command uses for trim.txt. It returns
// errWouldBlock when a different process holds the lock. A close of f
// releases the lock.
func tryLock(f *os.File) error {
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if !errors.Is(err, syscall.EINTR) {
			return err
		}
	}
}
