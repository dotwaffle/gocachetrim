// Package trim deletes entries from a Go build cache (GOCACHE) by age and
// by total size.
//
// It uses the same entry model as cmd/go/internal/cache. Each of the 256
// subdirectories holds "<hex>-a" action entries and "<hex>-d" output
// entries. A "-d" entry can also be a directory that holds one cached
// executable. When the go command uses an entry, it sets the mtime of the
// entry, at most once per hour.
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
	// Allocated is the disk space of an entry (st_blocks * 512).
	// Compression can make it less than Logical, and block overhead can
	// make it more.
	Allocated Metric = iota
	// Logical is the apparent size of an entry (st_size).
	Logical
)

// goTrimLimit is trimLimit from cmd/go/internal/cache. Run writes trim.txt
// only if MaxAge is more than 0 and not more than this limit. Then the trim
// selects all the entries that the daily trim of the go command deletes,
// so the go command can skip its trim.
const goTrimLimit = 5 * 24 * time.Hour

// readmePrefix is the first line of the README that the go command writes
// in each cache directory. Run refuses to work on a directory without it.
const readmePrefix = "This directory holds cached build artifacts from the Go build system."

// lockRetry is the time between attempts to lock trim.txt.
const lockRetry = 10 * time.Millisecond

// maxLoggedErrors is the number of reported trim errors that Run logs at
// the warning level. Run logs the errors after this number at the debug
// level.
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

// Plan is the result of Select. Entries shares storage with the input of
// Select. Treat Entries and the slices from Age and Size as read-only.
type Plan struct {
	// Entries holds all scanned entries, in compareAge order: oldest
	// first.
	Entries []Entry
	// Total is the total size of Entries.
	Total int64

	ageEnd  int // Entries[:ageEnd] are selected by MaxAge
	sizeEnd int // Entries[ageEnd:sizeEnd] are selected by MaxSize
}

// Age returns the entries that MaxAge selects, oldest first. The capacity
// of the result is its length, so an append to it cannot change Size.
func (p Plan) Age() []Entry { return p.Entries[:p.ageEnd:p.ageEnd] }

// Size returns the entries that MaxSize selects, oldest first.
func (p Plan) Size() []Entry { return p.Entries[p.ageEnd:p.sizeEnd:p.sizeEnd] }

// compareAge orders entries by mtime, oldest first. It orders entries with
// the same mtime by subdirectory and name, so that the order does not
// depend on the order of the scan.
func compareAge(a, b Entry) int {
	return cmp.Or(a.MTime.Compare(b.MTime), cmp.Compare(a.Sub, b.Sub), strings.Compare(a.Name, b.Name))
}

// Select sorts entries in place, oldest first, and returns the entries to
// delete. First it selects each entry with an mtime before now-maxAge.
// Then, if the other entries are larger than maxSize, it selects the
// oldest of them until the total is not more than maxSize. It does not
// select an entry with an mtime after now-minAge for maxSize. A zero
// maxAge or maxSize disables that step.
//
// Because the entries are oldest first, each step selects a range. The
// age step selects a prefix, and the size step selects the range after it.
func Select(entries []Entry, now time.Time, maxAge time.Duration, maxSize int64, minAge time.Duration) Plan {
	slices.SortFunc(entries, compareAge)
	p := Plan{Entries: entries, Total: sum(entries)}
	if maxAge > 0 {
		// The result is the index of the first entry with an mtime that
		// is not before the cutoff.
		p.ageEnd, _ = slices.BinarySearchFunc(entries, now.Add(-maxAge), func(e Entry, cutoff time.Time) int {
			return e.MTime.Compare(cutoff)
		})
	}
	p.sizeEnd = p.ageEnd
	if maxSize <= 0 {
		return p
	}
	remain := p.Total - sum(p.Age())
	floor := now.Add(-minAge)
	// The first entry after the floor ends the loop, because all the
	// entries after it are also newer than the floor.
	for _, e := range entries[p.ageEnd:] {
		if remain <= maxSize || e.MTime.After(floor) {
			break
		}
		remain -= e.Size
		p.sizeEnd++
	}
	return p
}

// Run trims the cache in cfg.Dir. It returns an error if it cannot start
// the trim, or if ctx is done. Result.Errors counts the errors for
// subdirectories, entries, and trim.txt.
//
// Run returns ErrNotCache if cfg.Dir does not have the README of a Go
// build cache. It returns ErrBusy if a different trim holds the lock on
// the cache.
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
	cfg.Log.Debug("scan done", "entries", len(entries), "age_victims", len(plan.Age()), "size_victims", len(plan.Size()))

	res := Result{Scanned: len(entries), ScannedBytes: plan.Total}
	if cfg.DryRun {
		res.AgeDeleted, res.AgeBytes = len(plan.Age()), sum(plan.Age())
		res.SizeDeleted, res.SizeBytes = len(plan.Size()), sum(plan.Size())
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

// errorLog counts reported trim errors and logs them. It is safe for
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
// workers calls at the same time. When ctx is done, it stops the calls for
// the remaining subdirectories, and waits for the active calls to return.
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
