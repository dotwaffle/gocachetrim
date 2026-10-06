package trim

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sync"
)

// removeAll deletes the entries in plan and adds the counts to res. Each
// worker deletes the entries of whole subdirectories, because unlink takes
// an exclusive lock on the parent directory. It stops when ctx is done.
func removeAll(ctx context.Context, root *os.Root, plan Plan, workers int, errs *errorLog, res *Result) {
	// Index the entries to delete by subdirectory. The indices of each
	// subdirectory are in ascending order, so a worker deletes the oldest
	// entries of its subdirectory first.
	victims := plan.Entries[:plan.sizeEnd]
	var bySub [256][]int
	for i, e := range victims {
		bySub[e.Sub] = append(bySub[e.Sub], i)
	}

	var mu sync.Mutex
	forEachSub(ctx, workers, func(sub uint8) {
		if len(bySub[sub]) == 0 {
			return
		}
		// Each worker counts in its own Result, and adds it to res once,
		// also when it stops early.
		var r Result
		defer func() {
			mu.Lock()
			res.AgeDeleted += r.AgeDeleted
			res.AgeBytes += r.AgeBytes
			res.SizeDeleted += r.SizeDeleted
			res.SizeBytes += r.SizeBytes
			res.Kept += r.Kept
			res.Gone += r.Gone
			res.GoneBytes += r.GoneBytes
			mu.Unlock()
		}()
		dir, err := openSub(root, sub)
		if errors.Is(err, fs.ErrNotExist) {
			for _, i := range bySub[sub] {
				r.Gone++
				r.GoneBytes += victims[i].Size
			}
			return
		}
		if err != nil {
			errs.report(err)
			return
		}
		defer func() { _ = dir.Close() }()
		for _, i := range bySub[sub] {
			if ctx.Err() != nil {
				break
			}
			removeOne(dir, victims[i], i >= plan.ageEnd, errs, &r)
		}
	})
}

// removeOne deletes the entry v from its subdirectory dir, unless the go
// command used it after the scan. bySize tells which limit selected v. An
// entry that is already gone counts as gone, not as deleted.
func removeOne(dir *os.Root, v Entry, bySize bool, errs *errorLog, r *Result) {
	fi, err := dir.Lstat(v.Name)
	if errors.Is(err, fs.ErrNotExist) {
		r.Gone++
		r.GoneBytes += v.Size
		return
	}
	if err != nil {
		errs.report(fmt.Errorf("%s: %w", subName(v.Sub), err))
		return
	}
	if fi.ModTime().After(v.MTime) {
		r.Kept++
		return
	}
	if fi.IsDir() {
		err = dir.RemoveAll(v.Name)
	} else {
		err = dir.Remove(v.Name)
	}
	if errors.Is(err, fs.ErrNotExist) {
		r.Gone++
		r.GoneBytes += v.Size
		return
	}
	if err != nil {
		errs.report(fmt.Errorf("%s: %w", subName(v.Sub), err))
		return
	}
	if bySize {
		r.SizeDeleted++
		r.SizeBytes += v.Size
	} else {
		r.AgeDeleted++
		r.AgeBytes += v.Size
	}
}
