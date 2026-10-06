// Package trim deletes entries from a Go build cache (GOCACHE) by age and
// by total size.
//
// It uses the same entry model as cmd/go/internal/cache: each of the 256
// subdirectories holds "<hex>-a" action entries and "<hex>-d" output
// entries, and the go command sets the mtime of an entry when it uses the
// entry, at most once per hour. A "-d" entry can also be a directory that
// holds one cached executable.
package trim

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Metric selects how the size of an entry is measured.
type Metric int

const (
	// Allocated is the disk space of an entry (st_blocks * 512). On a
	// filesystem with compression, this is less than Logical.
	Allocated Metric = iota
	// Logical is the apparent size of an entry (st_size).
	Logical
)

// goTrimLimit is trimLimit from cmd/go/internal/cache. When MaxAge is not
// more than this, a trim deletes at least as much as the go command's own
// daily trim, so Run tells the go command to skip its trim.
const goTrimLimit = 5 * 24 * time.Hour

// readmePrefix is the first line of the README that the go command writes
// in each cache directory. Run refuses to work on a directory without it.
const readmePrefix = "This directory holds cached build artifacts from the Go build system."

// lockRetry is the time between attempts to lock trim.txt.
const lockRetry = 10 * time.Millisecond

// maxLoggedErrors is the number of per-entry errors that Run logs at the
// warning level. Run logs the errors after this number at the debug level.
const maxLoggedErrors = 10

var (
	// ErrBusy means that a different trim holds the lock on the cache.
	ErrBusy = errors.New("cache is locked by a different trim")
	// ErrNotCache means that the directory is not a Go build cache.
	ErrNotCache = errors.New("not a Go build cache")
)

// Config is the configuration for one trim of one cache.
type Config struct {
	// Dir is the cache directory.
	Dir string
	// MaxAge is the age limit. A trim deletes each entry with an mtime
	// before Now-MaxAge. Zero disables the age limit.
	MaxAge time.Duration
	// MaxSize is the size limit. A trim deletes the oldest entries until
	// the total size is not more than MaxSize. Zero disables the size
	// limit.
	MaxSize int64
	// MinAge prevents the size limit from deleting an entry with an mtime
	// after Now-MinAge. It does not apply to MaxAge.
	MinAge time.Duration
	// Metric selects how the size of an entry is measured.
	Metric Metric
	// Workers is the number of goroutines that scan and delete. Each
	// goroutine works on whole subdirectories. A value less than 1 is 1.
	Workers int
	// DryRun selects the entries to delete, but does not delete them. The
	// Result then counts the selected entries as deleted.
	DryRun bool
	// Now is the time against which entry ages are measured.
	Now time.Time
	// Log receives warnings and debug messages. Nil discards them.
	Log *slog.Logger
}

// Result is the outcome of one trim.
type Result struct {
	Scanned      int   // entries found
	ScannedBytes int64 // total size of the entries found
	AgeDeleted   int   // entries deleted because of MaxAge
	AgeBytes     int64 // total size of the AgeDeleted entries
	SizeDeleted  int   // entries deleted because of MaxSize
	SizeBytes    int64 // total size of the SizeDeleted entries
	Kept         int   // entries not deleted because they were used after the scan
	Gone         int   // entries that a different process deleted after the scan
	GoneBytes    int64 // total size of the Gone entries
	Remaining    int64 // total size after the trim, from the scan and the deletes
	Shortfall    int64 // bytes over MaxSize after the trim
	Errors       int   // errors for entries, subdirectories, and trim.txt
}

// Entry is one cache entry found by a scan.
type Entry struct {
	Sub   uint8     // subdirectory, named "%02x"
	Name  string    // file name in the subdirectory
	MTime time.Time // time of the last use, as the go command records it
	Size  int64     // size in the configured Metric
}

// subName returns the name of a cache subdirectory, "00" to "ff".
func subName(sub uint8) string { return fmt.Sprintf("%02x", sub) }

// Plan is the set of entries that a trim deletes.
type Plan struct {
	Age   []Entry // entries older than MaxAge
	Size  []Entry // oldest remaining entries, deleted to meet MaxSize
	Total int64   // total size of all scanned entries
}

// Select returns the entries to delete. First it selects each entry with
// an mtime before now-maxAge. Then, if the other entries are larger than
// maxSize, it selects the oldest of them until the total is not more than
// maxSize, but no entry with an mtime after now-minAge. A zero maxAge or
// maxSize disables that step.
//
// Select does not change entries. Plan.Age is in the input order.
// Plan.Size is in mtime order, oldest first, and entries with the same
// mtime are in subdirectory and name order.
func Select(entries []Entry, now time.Time, maxAge time.Duration, maxSize int64, minAge time.Duration) Plan {
	var p Plan
	var keep []Entry
	var remain int64
	ageCutoff := now.Add(-maxAge)
	for _, e := range entries {
		p.Total += e.Size
		if maxAge > 0 && e.MTime.Before(ageCutoff) {
			p.Age = append(p.Age, e)
			continue
		}
		keep = append(keep, e)
		remain += e.Size
	}
	if maxSize <= 0 || remain <= maxSize {
		return p
	}

	slices.SortFunc(keep, func(a, b Entry) int {
		return cmp.Or(a.MTime.Compare(b.MTime), cmp.Compare(a.Sub, b.Sub), strings.Compare(a.Name, b.Name))
	})
	floor := now.Add(-minAge)
	// keep is in mtime order, so the first entry after the floor ends the
	// loop: all the entries after it are also newer than the floor.
	for _, e := range keep {
		if remain <= maxSize || e.MTime.After(floor) {
			break
		}
		p.Size = append(p.Size, e)
		remain -= e.Size
	}
	return p
}

// Run trims the cache in cfg.Dir. It returns an error only when it cannot
// trim at all. It reports the errors for individual entries in
// Result.Errors.
//
// Run returns ErrNotCache if cfg.Dir does not have the README of a Go build
// cache, and ErrBusy if a different trim holds the lock on the cache.
//
// If ctx is done during the scan, Run returns an empty Result and
// ctx.Err(). If ctx is done during the deletes, Run stops, and returns the
// counts of the deletes that it completed with ctx.Err(). In both cases it
// does not write trim.txt.
func Run(ctx context.Context, cfg Config) (Result, error) {
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.DiscardHandler)
	}
	cfg.Workers = max(cfg.Workers, 1)
	root, err := os.OpenRoot(cfg.Dir)
	if err != nil {
		return Result{}, err
	}
	defer func() { _ = root.Close() }()
	err = checkCache(root)
	if err != nil {
		return Result{}, err
	}
	unlock, err := lockDir(root)
	if err != nil {
		return Result{}, err
	}
	defer unlock()

	errs := &errorLog{log: cfg.Log}
	entries := scan(ctx, root, cfg.Metric, cfg.Workers, errs)
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	plan := Select(entries, cfg.Now, cfg.MaxAge, cfg.MaxSize, cfg.MinAge)
	cfg.Log.Debug("scan done", "entries", len(entries), "age_victims", len(plan.Age), "size_victims", len(plan.Size))

	res := Result{Scanned: len(entries), ScannedBytes: plan.Total}
	if cfg.DryRun {
		res.AgeDeleted, res.AgeBytes = len(plan.Age), sum(plan.Age)
		res.SizeDeleted, res.SizeBytes = len(plan.Size), sum(plan.Size)
	} else {
		removeAll(ctx, root, plan, cfg.Workers, errs, &res)
	}
	res.Remaining = res.ScannedBytes - res.AgeBytes - res.SizeBytes - res.GoneBytes
	if cfg.MaxSize > 0 {
		res.Shortfall = max(res.Remaining-cfg.MaxSize, 0)
	}

	if !cfg.DryRun && ctx.Err() == nil && cfg.MaxAge > 0 && cfg.MaxAge <= goTrimLimit {
		if err := writeTrimTime(ctx, root, cfg.Now); err != nil {
			errs.report(fmt.Errorf("write trim.txt: %w", err))
		}
	}
	res.Errors = int(errs.n.Load())
	return res, ctx.Err()
}

// sum returns the total size of entries.
func sum(entries []Entry) int64 {
	var n int64
	for _, e := range entries {
		n += e.Size
	}
	return n
}

// checkCache makes sure that root has the README of a Go build cache, so a
// wrong directory argument cannot cause deletions.
func checkCache(root *os.Root) error {
	b, err := root.ReadFile("README")
	if err != nil || !strings.HasPrefix(string(b), readmePrefix) {
		return fmt.Errorf("%s: %w (no Go cache README)", root.Name(), ErrNotCache)
	}
	return nil
}

// writeTrimTime records now as the time of the last trim in trim.txt, in
// the format and under the lock that the go command uses. The go command
// then skips its own trim for 24 hours. If ctx is done before it gets the
// lock, it does not write the file.
func writeTrimTime(ctx context.Context, root *os.Root, now time.Time) error {
	f, err := root.OpenFile("trim.txt", os.O_RDWR|os.O_CREATE, 0o666)
	if err != nil {
		return err
	}
	b := strconv.AppendInt(nil, now.Unix(), 10)
	err = lockWait(ctx, f)
	if err == nil {
		_, err = f.WriteAt(b, 0)
	}
	if err == nil {
		err = f.Truncate(int64(len(b)))
	}
	return errors.Join(err, f.Close())
}

// lockWait takes an exclusive lock on f. It tries again until it gets the
// lock or ctx is done, so that a cancel does not wait for a different
// process to release the lock.
func lockWait(ctx context.Context, f *os.File) error {
	for {
		err := tryLock(f)
		if !errors.Is(err, errWouldBlock) {
			if err == nil {
				err = ctx.Err()
			}
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(lockRetry):
		}
	}
}

// lockDir takes a non-blocking exclusive lock on the cache directory
// itself. The go command never locks the cache directory, so this lock
// excludes only other trims.
func lockDir(root *os.Root) (unlock func(), err error) {
	f, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	if err := tryLock(f); err != nil {
		_ = f.Close()
		if errors.Is(err, errWouldBlock) {
			return nil, ErrBusy
		}
		return nil, fmt.Errorf("lock %s: %w", root.Name(), err)
	}
	return func() { _ = f.Close() }, nil
}

// errorLog counts per-entry errors and logs them. It is safe for
// concurrent use.
type errorLog struct {
	log *slog.Logger
	n   atomic.Int64
}

// report counts err, and logs it at the warning level for the first
// maxLoggedErrors errors and at the debug level after that.
func (l *errorLog) report(err error) {
	if l.n.Add(1) <= maxLoggedErrors {
		l.log.Warn("entry error", "err", err)
		return
	}
	l.log.Debug("entry error", "err", err)
}

// forEachSub calls fn for each of the 256 subdirectories, with at most
// workers calls at the same time. It stops when ctx is done.
func forEachSub(ctx context.Context, workers int, fn func(sub uint8)) {
	subs := make(chan uint8)
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			for sub := range subs {
				fn(sub)
			}
		})
	}
send:
	for i := range 256 {
		select {
		case subs <- uint8(i):
		case <-ctx.Done():
			break send
		}
	}
	close(subs)
	wg.Wait()
}
