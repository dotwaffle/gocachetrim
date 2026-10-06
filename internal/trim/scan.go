package trim

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"strings"
)

// isEntryName reports whether name is a cache entry name: 64 lowercase hex
// digits, then "-a" or "-d".
func isEntryName(name string) bool {
	if len(name) != 66 || name[64] != '-' || (name[65] != 'a' && name[65] != 'd') {
		return false
	}
	for _, c := range name[:64] {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// scan returns the entries in all 256 subdirectories of the cache. It
// ignores all other files. It reports errors for subdirectories and
// entries to errs, and does not stop for them.
func scan(ctx context.Context, root *os.Root, m Metric, workers int, errs *errorLog) []Entry {
	var bySub [256][]Entry
	forEachSub(ctx, workers, func(sub uint8) {
		bySub[sub] = scanSub(root, sub, m, errs)
	})
	return slices.Concat(bySub[:]...)
}

// scanSub returns the entries in one subdirectory. A subdirectory that
// does not exist has no entries.
func scanSub(root *os.Root, sub uint8, m Metric, errs *errorLog) []Entry {
	dir, err := openSub(root, sub)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			errs.report(err)
		}
		return nil
	}
	defer func() { _ = dir.Close() }()
	names, err := readNames(dir)
	if err != nil {
		errs.report(fmt.Errorf("read %s: %w", subName(sub), err))
		// readNames returns the names that it read before the error.
	}
	var out []Entry
	for _, name := range names {
		if !isEntryName(name) {
			continue
		}
		fi, err := dir.Lstat(name)
		if errors.Is(err, fs.ErrNotExist) {
			continue // deleted after the read
		}
		if err != nil {
			errs.report(fmt.Errorf("%s: %w", subName(sub), err))
			continue
		}
		size, ok := entrySize(dir, name, fi, m, errs)
		if !ok {
			continue
		}
		out = append(out, Entry{Sub: sub, Name: name, MTime: fi.ModTime(), Size: size})
	}
	return out
}

// openSub opens a subdirectory of the cache as a root, so that no symbolic
// link can move a later operation outside of the cache.
func openSub(root *os.Root, sub uint8) (*os.Root, error) {
	return root.OpenRoot(subName(sub))
}

// readNames returns the names in dir. On an error, it also returns the
// names that it read before the error.
func readNames(dir *os.Root) ([]string, error) {
	f, err := dir.Open(".")
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return f.Readdirnames(-1)
}

// entrySize returns the size of the entry name in dir. A regular file is
// one entry. A "-d" directory is one cached executable, and its size
// includes the files in it. It returns false for other file types, which
// the go command does not make.
func entrySize(dir *os.Root, name string, fi fs.FileInfo, m Metric, errs *errorLog) (int64, bool) {
	switch {
	case fi.Mode().IsRegular():
		return sizeOf(fi, m), true
	case fi.IsDir() && strings.HasSuffix(name, "-d"):
		size := sizeOf(fi, m)
		exe, err := dir.OpenRoot(name)
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				errs.report(err)
			}
			return size, true
		}
		defer func() { _ = exe.Close() }()
		names, err := readNames(exe)
		if err != nil {
			errs.report(fmt.Errorf("read %s: %w", name, err))
		}
		for _, n := range names {
			if fi, err := exe.Lstat(n); err == nil {
				size += sizeOf(fi, m)
			}
		}
		return size, true
	}
	return 0, false
}

// sizeOf returns the size of one file in the metric m.
func sizeOf(fi fs.FileInfo, m Metric) int64 {
	if m == Allocated {
		return allocated(fi)
	}
	return fi.Size()
}
