package trim

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sync"
)

// victim is an entry to delete, with the limit that selected it.
type victim struct {
	Entry
	bySize bool
}

// removeAll deletes the entries in plan and adds the counts to res. Each
// worker deletes the entries of whole subdirectories, because unlink takes
// an exclusive lock on the parent directory. It stops when ctx is done.
func removeAll(ctx context.Context, root *os.Root, plan Plan, workers int, errs *errorLog, res *Result) {
	var bySub [256][]victim
	for _, e := range plan.Age {
		bySub[e.Sub] = append(bySub[e.Sub], victim{e, false})
	}
	for _, e := range plan.Size {
		bySub[e.Sub] = append(bySub[e.Sub], victim{e, true})
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
			for _, v := range bySub[sub] {
				r.Gone++
				r.GoneBytes += v.Size
			}
			return
		}
		if err != nil {
			errs.report(err)
			return
		}
		defer func() { _ = dir.Close() }()
		for _, v := range bySub[sub] {
			if ctx.Err() != nil {
				break
			}
			removeOne(dir, v, errs, &r)
		}
	})
}

// removeOne deletes one entry from its subdirectory dir, unless the go
// command used it after the scan. An entry that is already gone counts as
// gone, not as deleted.
func removeOne(dir *os.Root, v victim, errs *errorLog, r *Result) {
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
	if v.bySize {
		r.SizeDeleted++
		r.SizeBytes += v.Size
	} else {
		r.AgeDeleted++
		r.AgeBytes += v.Size
	}
}
