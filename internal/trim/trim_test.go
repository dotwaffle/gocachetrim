package trim

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
	"time"
)

var now = time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)

const day = 24 * time.Hour

// newCache makes an empty cache directory with a README and 256
// subdirectories, like the go command does.
func newCache(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "README"), []byte(readmePrefix+"\n"), 0o666); err != nil {
		t.Fatal(err)
	}
	for i := range 256 {
		if err := os.Mkdir(filepath.Join(dir, subName(uint8(i))), 0o777); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func entryName(i int, kind byte) string { return fmt.Sprintf("%064x-%c", i, kind) }

// put writes a file of size bytes at dir/rel with an mtime of now-age.
func put(t *testing.T, dir, rel string, size int, age time.Duration) string {
	t.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, make([]byte, size), 0o666); err != nil {
		t.Fatal(err)
	}
	setAge(t, p, age)
	return p
}

func setAge(t *testing.T, p string, age time.Duration) {
	t.Helper()
	mt := now.Add(-age)
	if err := os.Chtimes(p, mt, mt); err != nil {
		t.Fatal(err)
	}
}

func openRoot(t *testing.T, dir string) *os.Root {
	t.Helper()
	r, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	return r
}

func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

func baseConfig(dir string) Config {
	return Config{Dir: dir, Metric: Logical, Workers: 4, Now: now, MinAge: 2 * time.Hour}
}

func TestIsEntryName(t *testing.T) {
	hex := fmt.Sprintf("%064x", 1)
	tests := []struct {
		name string
		want bool
	}{
		{hex + "-a", true},
		{hex + "-d", true},
		{hex + "-x", false},
		{hex + "a", false},
		{hex[1:] + "-a", false},
		{"A" + hex[1:] + "-a", false},
		{"g" + hex[1:] + "-d", false},
		{hex + "-dd", false},
		{"README", false},
	}
	for _, tt := range tests {
		if got := isEntryName(tt.name); got != tt.want {
			t.Errorf("isEntryName(%q) = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestSelect(t *testing.T) {
	e := func(id int, age time.Duration, size int64) Entry {
		return Entry{Sub: uint8(id), Name: entryName(id, 'd'), MTime: now.Add(-age), Size: size}
	}
	entries := []Entry{
		e(1, 10*time.Minute, 10),
		e(2, 3*time.Hour, 10),
		e(3, 5*time.Hour, 10),
		e(4, 2*day, 10),
		e(5, 4*day, 10),
	}
	ids := func(es []Entry) []int {
		var out []int
		for _, e := range es {
			out = append(out, int(e.Sub))
		}
		return out
	}
	tests := []struct {
		name     string
		maxAge   time.Duration
		maxSize  int64
		minAge   time.Duration
		wantAge  []int
		wantSize []int
	}{
		{name: "no limits"},
		{name: "age only", maxAge: 3 * day, wantAge: []int{5}},
		{name: "size oldest first", maxSize: 25, minAge: time.Hour, wantSize: []int{5, 4, 3}},
		{name: "size under limit", maxSize: 50},
		{name: "size at limit", maxSize: 50, minAge: time.Hour},
		{name: "min age stops size", maxSize: 5, minAge: 4 * time.Hour, wantSize: []int{5, 4, 3}},
		{name: "age then size", maxAge: 3 * day, maxSize: 20, minAge: time.Hour, wantAge: []int{5}, wantSize: []int{4, 3}},
		{name: "age satisfies size", maxAge: 1 * day, maxSize: 30, minAge: time.Hour, wantAge: []int{4, 5}},
		{name: "min age equal to entry age", maxSize: 5, minAge: 3 * time.Hour, wantSize: []int{5, 4, 3, 2}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := slices.Clone(entries)
			p := Select(in, now, tt.maxAge, tt.maxSize, tt.minAge)
			if got := ids(p.Age); !slices.Equal(got, tt.wantAge) {
				t.Errorf("Age = %v, want %v", got, tt.wantAge)
			}
			if got := ids(p.Size); !slices.Equal(got, tt.wantSize) {
				t.Errorf("Size = %v, want %v", got, tt.wantSize)
			}
			if p.Total != 50 {
				t.Errorf("Total = %d, want 50", p.Total)
			}
			if !slices.Equal(in, entries) {
				t.Errorf("Select changed its input")
			}
		})
	}
}

func TestSelectTieOrder(t *testing.T) {
	mt := now.Add(-day)
	entries := []Entry{
		{Sub: 2, Name: entryName(1, 'd'), MTime: mt, Size: 1},
		{Sub: 1, Name: entryName(2, 'd'), MTime: mt, Size: 1},
		{Sub: 1, Name: entryName(1, 'd'), MTime: mt, Size: 1},
	}
	p := Select(entries, now, 0, 1, 0)
	want := []Entry{entries[2], entries[1]}
	if !slices.Equal(p.Size, want) {
		t.Errorf("Size = %v, want %v", p.Size, want)
	}
}

func TestRunAgeAndSize(t *testing.T) {
	dir := newCache(t)
	fresh := put(t, dir, "00/"+entryName(1, 'a'), 100, 10*time.Minute)
	recent := put(t, dir, "01/"+entryName(2, 'd'), 100, 3*time.Hour)
	older := put(t, dir, "02/"+entryName(3, 'd'), 100, 2*day)
	oldest := put(t, dir, "ff/"+entryName(4, 'd'), 100, 4*day)

	cfg := baseConfig(dir)
	cfg.MaxAge = 3 * day
	cfg.MaxSize = 150
	res, err := Run(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	want := Result{
		Scanned: 4, ScannedBytes: 400,
		AgeDeleted: 1, AgeBytes: 100,
		SizeDeleted: 2, SizeBytes: 200,
		Remaining: 100,
	}
	if res != want {
		t.Errorf("Run = %+v, want %+v", res, want)
	}
	for p, want := range map[string]bool{fresh: true, recent: false, older: false, oldest: false} {
		if exists(p) != want {
			t.Errorf("exists(%s) = %v, want %v", filepath.Base(p), !want, want)
		}
	}
}

func TestRunShortfall(t *testing.T) {
	dir := newCache(t)
	put(t, dir, "00/"+entryName(1, 'd'), 100, 10*time.Minute)
	put(t, dir, "01/"+entryName(2, 'd'), 100, 30*time.Minute)
	old := put(t, dir, "02/"+entryName(3, 'd'), 100, 5*time.Hour)

	cfg := baseConfig(dir)
	cfg.MaxSize = 50
	res, err := Run(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if res.SizeDeleted != 1 || res.Remaining != 200 || res.Shortfall != 150 {
		t.Errorf("Run = %+v, want 1 deleted, 200 remaining, 150 shortfall", res)
	}
	if exists(old) {
		t.Errorf("old entry not deleted")
	}
}

func TestRunIgnoresOtherFiles(t *testing.T) {
	dir := newCache(t)
	old := 30 * day
	keep := []string{
		put(t, dir, "fuzz/pkg/FuzzX/"+entryName(1, 'd'), 10, old),
		put(t, dir, "testexpire.txt", 10, old),
		put(t, dir, "00/"+entryName(2, 'x'), 10, old),
		put(t, dir, "00/"+entryName(3, 'd')+".tmp", 10, old),
		put(t, dir, "zz/"+entryName(4, 'd'), 10, old),
		put(t, dir, entryName(5, 'd'), 10, old),
	}
	// A symlink with an entry name is not a cache entry.
	link := filepath.Join(dir, "01", entryName(6, 'a'))
	if err := os.Symlink(keep[1], link); err != nil {
		t.Fatal(err)
	}
	keep = append(keep, link)
	// An "-a" directory is not a cache entry.
	adir := filepath.Join(dir, "02", entryName(7, 'a'))
	put(t, adir, "x", 10, old)
	setAge(t, adir, old)
	keep = append(keep, adir)

	cfg := baseConfig(dir)
	cfg.MaxAge = day
	cfg.MaxSize = 1
	cfg.MinAge = 0
	res, err := Run(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if res.Scanned != 0 {
		t.Errorf("Scanned = %d, want 0", res.Scanned)
	}
	for _, p := range append(keep, filepath.Join(dir, "README")) {
		if !exists(p) {
			t.Errorf("%s was deleted", p)
		}
	}
}

func TestRunExecutable(t *testing.T) {
	dir := newCache(t)
	exe := filepath.Join(dir, "03", entryName(1, 'd'))
	put(t, exe, "prog", 1000, 4*day)
	setAge(t, exe, 4*day)

	cfg := baseConfig(dir)
	cfg.MaxAge = 3 * day
	res, err := Run(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if res.Scanned != 1 || res.AgeDeleted != 1 || res.ScannedBytes < 1000 {
		t.Errorf("Run = %+v, want one entry of at least 1000 bytes deleted", res)
	}
	if exists(exe) {
		t.Errorf("executable entry not deleted")
	}
}

func TestRunAllocated(t *testing.T) {
	dir := newCache(t)
	const size = 64 << 10
	p := filepath.Join(dir, "00", entryName(1, 'd'))
	// Random bytes, so that no filesystem can compress the file or store
	// it as a hole.
	b := make([]byte, size)
	rand.NewChaCha8([32]byte{}).Read(b)
	if err := os.WriteFile(p, b, 0o666); err != nil {
		t.Fatal(err)
	}
	cfg := baseConfig(dir)
	cfg.Metric = Allocated
	res, err := Run(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if res.ScannedBytes < size || res.ScannedBytes > size+1<<20 {
		t.Errorf("allocated size = %d, want %d plus less than 1MiB of overhead", res.ScannedBytes, size)
	}
}

func TestRunTrimTime(t *testing.T) {
	tests := []struct {
		name   string
		maxAge time.Duration
		dryRun bool
		want   bool
	}{
		{name: "short max age", maxAge: day, want: true},
		{name: "go trim limit", maxAge: 5 * day, want: true},
		{name: "long max age", maxAge: 7 * day},
		{name: "no max age"},
		{name: "dry run", maxAge: day, dryRun: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := newCache(t)
			trimTxt := put(t, dir, "trim.txt", 0, 0)
			if err := os.WriteFile(trimTxt, []byte("1000000000000"), 0o666); err != nil {
				t.Fatal(err)
			}
			cfg := baseConfig(dir)
			cfg.MaxAge = tt.maxAge
			cfg.DryRun = tt.dryRun
			if _, err := Run(t.Context(), cfg); err != nil {
				t.Fatal(err)
			}
			b, err := os.ReadFile(trimTxt)
			if err != nil {
				t.Fatal(err)
			}
			if got := string(b) == strconv.FormatInt(now.Unix(), 10); got != tt.want {
				t.Errorf("trim.txt = %q, want written %v", b, tt.want)
			}
		})
	}
}

func TestRunDryRun(t *testing.T) {
	dir := newCache(t)
	p := put(t, dir, "00/"+entryName(1, 'd'), 100, 4*day)
	cfg := baseConfig(dir)
	cfg.MaxAge = day
	cfg.DryRun = true
	res, err := Run(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if res.AgeDeleted != 1 || res.AgeBytes != 100 || res.Remaining != 0 {
		t.Errorf("Run = %+v, want one entry reported", res)
	}
	if !exists(p) {
		t.Errorf("dry run deleted an entry")
	}
}

func TestRunErrors(t *testing.T) {
	dir := newCache(t)
	unlock, err := lockDir(openRoot(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(unlock)

	tests := []struct {
		name     string
		canceled bool
		dir      string
		want     error
	}{
		{name: "busy", dir: dir, want: ErrBusy},
		{name: "not a cache", dir: t.TempDir(), want: ErrNotCache},
		{name: "canceled", canceled: true, dir: newCache(t), want: context.Canceled},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if tt.canceled {
				cancel()
			}
			_, err := Run(ctx, baseConfig(tt.dir))
			if !errors.Is(err, tt.want) {
				t.Errorf("Run error = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestRemoveKeepsUsedEntry(t *testing.T) {
	dir := newCache(t)
	p := put(t, dir, "00/"+entryName(1, 'd'), 100, time.Hour)
	// The scan saw an older mtime, so the go command used the entry after
	// the scan.
	v := victim{Entry: Entry{Sub: 0, Name: entryName(1, 'd'), MTime: now.Add(-day), Size: 100}}
	var r Result
	errs := &errorLog{log: slog.New(slog.DiscardHandler)}
	sub := openRoot(t, filepath.Join(dir, "00"))
	removeOne(sub, v, errs, &r)
	if r.Kept != 1 || r.AgeDeleted != 0 || !exists(p) {
		t.Errorf("removeOne = %+v, exists %v; want entry kept", r, exists(p))
	}

	// An entry that is already gone is not an error and not a deletion.
	v.Name = entryName(2, 'd')
	r = Result{}
	removeOne(sub, v, errs, &r)
	if r != (Result{Gone: 1, GoneBytes: 100}) || errs.n.Load() != 0 {
		t.Errorf("removeOne of missing entry = %+v, %d errors", r, errs.n.Load())
	}
}

func TestRunStaysInCache(t *testing.T) {
	dir := newCache(t)
	outside := t.TempDir()
	victim := put(t, outside, entryName(1, 'd'), 100, 30*day)
	// A subdirectory that is a symbolic link to a directory outside of the
	// cache is not followed.
	sub := filepath.Join(dir, "05")
	if err := os.Remove(sub); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, sub); err != nil {
		t.Fatal(err)
	}
	cfg := baseConfig(dir)
	cfg.MaxAge = day
	res, err := Run(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if res.Scanned != 0 || res.Errors != 1 {
		t.Errorf("Run = %+v, want 0 scanned and 1 error", res)
	}
	if !exists(victim) {
		t.Errorf("entry outside of the cache was deleted")
	}
}

func TestRemoveAllGoneSubdir(t *testing.T) {
	dir := newCache(t)
	if err := os.Remove(filepath.Join(dir, "07")); err != nil {
		t.Fatal(err)
	}
	plan := Plan{Size: []Entry{{Sub: 7, Name: entryName(1, 'd'), MTime: now.Add(-day), Size: 100}}}
	var res Result
	errs := &errorLog{log: slog.New(slog.DiscardHandler)}
	removeAll(t.Context(), openRoot(t, dir), plan, 2, errs, &res)
	if res != (Result{Gone: 1, GoneBytes: 100}) || errs.n.Load() != 0 {
		t.Errorf("removeAll = %+v, %d errors; want 1 gone", res, errs.n.Load())
	}
}

func TestWriteTrimTimeCancel(t *testing.T) {
	dir := newCache(t)
	root := openRoot(t, dir)
	p := put(t, dir, "trim.txt", 0, 0)
	if err := os.WriteFile(p, []byte("123"), 0o666); err != nil {
		t.Fatal(err)
	}
	// A different open file description holds the lock, like a different
	// process.
	holder, err := os.OpenFile(p, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := tryLock(holder); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if err := writeTrimTime(ctx, root, now); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("writeTrimTime with a held lock = %v, want %v", err, context.DeadlineExceeded)
	}
	if b, _ := os.ReadFile(p); string(b) != "123" {
		t.Errorf("trim.txt = %q after a cancel, want unchanged", b)
	}

	// After the lock is released, the write succeeds.
	go func() {
		time.Sleep(3 * lockRetry)
		_ = holder.Close()
	}()
	if err := writeTrimTime(t.Context(), root, now); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(p); string(b) != strconv.FormatInt(now.Unix(), 10) {
		t.Errorf("trim.txt = %q, want %d", b, now.Unix())
	}
}
