// Command gocachetrim trims a Go build cache (GOCACHE) by entry age and by
// total size. It runs once, or as a daemon that trims at an interval.
//
// The go command marks an entry as used by setting its mtime, at most once
// per hour. gocachetrim deletes each entry with an mtime before now minus
// -max-age. Then, if the cache is larger than -max-size, it deletes the
// oldest entries until the cache is not larger, but no entry with an mtime
// after now minus -min-age. The -min-age limit protects the entries of
// running builds, but it is best effort. The README explains the limits.
//
// Run "gocachetrim -h" for the flags.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/dotwaffle/gocachetrim/internal/trim"
)

// day is the duration of the "d" suffix in a duration flag.
const day = 24 * time.Hour

// options holds the command-line flags.
type options struct {
	cache    string
	maxAge   durationFlag
	maxSize  sizeFlag
	minAge   durationFlag
	metric   string
	workers  int
	daemon   bool
	interval durationFlag
	dryRun   bool
	verbose  bool
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stderr)
	stop()
	os.Exit(code)
}

// run executes the command and returns its exit status: 0 for success, 1
// for a trim error, and 2 for a usage error.
func run(ctx context.Context, args []string, stderr io.Writer) int {
	opts := options{
		maxAge:   durationFlag(3 * day),
		maxSize:  sizeFlag(10 << 30),
		minAge:   durationFlag(2 * time.Hour),
		metric:   "allocated",
		workers:  8,
		interval: durationFlag(time.Hour),
	}
	fs := flag.NewFlagSet("gocachetrim", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		_, _ = fmt.Fprintf(fs.Output(), "usage: gocachetrim [flags]\n\nTrim the Go build cache by entry age and total size.\n\n")
		fs.PrintDefaults()
	}
	fs.StringVar(&opts.cache, "cache", "", "cache `dir` (default: go env GOCACHE)")
	fs.Var(&opts.maxAge, "max-age", "delete entries not used for this `duration` (0 disables)")
	fs.Var(&opts.maxSize, "max-size", "delete the oldest entries until the cache is not larger than this `size` (0 disables)")
	fs.Var(&opts.minAge, "min-age", "never delete entries used within this `duration` to meet -max-size")
	fs.StringVar(&opts.metric, "size-metric", opts.metric, "measure sizes as `allocated` disk space or logical file size")
	fs.IntVar(&opts.workers, "workers", opts.workers, "number of concurrent scan and delete workers")
	fs.BoolVar(&opts.daemon, "daemon", false, "trim now, then again at each -interval, until SIGINT or SIGTERM")
	fs.Var(&opts.interval, "interval", "time between trims in daemon mode")
	fs.BoolVar(&opts.dryRun, "dry-run", false, "report what would be deleted, but do not delete")
	fs.BoolVar(&opts.verbose, "v", false, "log debug messages")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	metric, err := opts.validate(fs.Args())
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "gocachetrim: %v\n", err)
		fs.Usage()
		return 2
	}

	level := slog.LevelInfo
	if opts.verbose {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: level}))
	if opts.maxSize > 0 && time.Duration(opts.minAge) < time.Hour {
		log.Warn("min-age is less than 1h, so -max-size can delete entries that a running build uses",
			slog.String("min_age", opts.minAge.String()))
	}

	dir := opts.cache
	if dir == "" {
		if dir, err = goCacheDir(ctx); err != nil {
			log.Error("find cache", slog.Any("err", err))
			return 1
		}
	}
	cfg := trim.Config{
		Dir:     dir,
		MaxAge:  time.Duration(opts.maxAge),
		MaxSize: int64(opts.maxSize),
		MinAge:  time.Duration(opts.minAge),
		Metric:  metric,
		Workers: opts.workers,
		DryRun:  opts.dryRun,
		Log:     log.With(slog.String("cache", dir)),
	}
	if !opts.daemon {
		if !trimOnce(ctx, cfg) {
			return 1
		}
		return 0
	}
	return daemon(ctx, cfg, time.Duration(opts.interval))
}

// validate checks the flags and the arguments, and returns the size metric.
func (o *options) validate(args []string) (trim.Metric, error) {
	var metric trim.Metric
	switch o.metric {
	case "allocated":
		metric = trim.Allocated
	case "logical":
		metric = trim.Logical
	default:
		return 0, fmt.Errorf("-size-metric must be allocated or logical, not %q", o.metric)
	}
	switch {
	case len(args) > 0:
		return 0, fmt.Errorf("unexpected arguments: %q", args)
	case o.maxAge == 0 && o.maxSize == 0:
		return 0, errors.New("-max-age and -max-size are both 0, so there is nothing to do")
	case o.maxAge > 0 && o.maxAge < o.minAge:
		return 0, fmt.Errorf("-max-age %v is less than -min-age %v", &o.maxAge, &o.minAge)
	case o.workers < 1:
		return 0, errors.New("-workers must be at least 1")
	case o.daemon && o.interval <= 0:
		return 0, errors.New("-interval must be more than 0")
	}
	return metric, nil
}

// durationFlag is a flag.Value for a duration. It accepts the
// time.ParseDuration syntax, and also a number of days with a "d" suffix,
// for example "3d" or "1.5d".
type durationFlag time.Duration

func (d *durationFlag) String() string { return formatDuration(time.Duration(*d)) }

func (d *durationFlag) Set(s string) error {
	v, err := parseDuration(s)
	if err != nil {
		return err
	}
	*d = durationFlag(v)
	return nil
}

// parseDuration parses the syntax that durationFlag accepts. It rejects a
// negative duration.
func parseDuration(s string) (time.Duration, error) {
	var d time.Duration
	if num, ok := strings.CutSuffix(s, "d"); ok {
		f, err := strconv.ParseFloat(num, 64)
		// float64(math.MaxInt64) rounds up to 2^63, so ">=" rejects each
		// value that does not fit in an int64.
		if err != nil || math.IsNaN(f) || f*float64(day) >= math.MaxInt64 {
			return 0, fmt.Errorf("invalid duration %q", s)
		}
		d = time.Duration(f * float64(day))
	} else {
		var err error
		if d, err = time.ParseDuration(s); err != nil {
			return 0, err
		}
	}
	if d < 0 {
		return 0, fmt.Errorf("negative duration %q", s)
	}
	return d, nil
}

// formatDuration formats a whole number of days with a "d" suffix, and
// other durations with time.Duration.String.
func formatDuration(d time.Duration) string {
	if d != 0 && d%day == 0 {
		return fmt.Sprintf("%dd", d/day)
	}
	return d.String()
}

// sizeFlag is a flag.Value for a size in bytes. It accepts a number with
// an optional K, M, G or T suffix for powers of 1024, and an optional "B"
// or "iB" after the suffix, for example "10G", "512MiB" or "1.5T".
type sizeFlag int64

func (s *sizeFlag) String() string { return formatSize(int64(*s)) }

func (s *sizeFlag) Set(v string) error {
	n, err := parseSize(v)
	if err != nil {
		return err
	}
	*s = sizeFlag(n)
	return nil
}

// errSize is the error of parseSize for a size that it cannot parse.
var errSize = errors.New("invalid size")

// parseSize parses the syntax that sizeFlag accepts. It rejects a negative
// size and a size that does not fit in an int64.
func parseSize(s string) (int64, error) {
	num := strings.ToUpper(strings.TrimSpace(s))
	if n, ok := strings.CutSuffix(num, "IB"); ok {
		num = n
	} else {
		num = strings.TrimSuffix(num, "B")
	}
	mult := 1.0
	if num != "" {
		if i := strings.IndexByte("KMGT", num[len(num)-1]); i >= 0 {
			mult = math.Pow(1024, float64(i+1))
			num = num[:len(num)-1]
		}
	}
	f, err := strconv.ParseFloat(num, 64)
	// float64(math.MaxInt64) rounds up to 2^63, so ">=" rejects each value
	// that does not fit in an int64.
	if err != nil || math.IsNaN(f) || f < 0 || f*mult >= math.MaxInt64 {
		return 0, fmt.Errorf("%w %q", errSize, s)
	}
	return int64(f * mult), nil
}

// formatSize formats n in bytes if it is less than 1KiB. Otherwise it uses
// the largest unit, up to TiB, that makes the value at least 1, with one
// decimal place.
func formatSize(n int64) string {
	const units = "KMGT"
	if n < 1024 {
		return strconv.FormatInt(n, 10) + "B"
	}
	f := float64(n)
	i := -1
	for f >= 1024 && i < len(units)-1 {
		f /= 1024
		i++
	}
	return strconv.FormatFloat(f, 'f', 1, 64) + string(units[i]) + "iB"
}

// goCacheDir returns the cache directory from "go env". It fails when the
// cache is off or a GOCACHEPROG program manages it.
func goCacheDir(ctx context.Context) (string, error) {
	out, err := exec.CommandContext(ctx, "go", "env", "GOCACHE", "GOCACHEPROG").Output()
	if err != nil {
		return "", fmt.Errorf("go env: %w", err)
	}
	dir, prog, _ := strings.Cut(strings.TrimRight(string(out), "\n"), "\n")
	switch {
	case prog != "":
		return "", fmt.Errorf("GOCACHEPROG is set (%s), so the go command does not use a cache directory", prog)
	case dir == "" || dir == "off":
		return "", errors.New("GOCACHE is off")
	}
	return dir, nil
}

// daemon trims at each interval until ctx is done.
func daemon(ctx context.Context, cfg trim.Config, interval time.Duration) int {
	cfg.Log.Info("daemon started", slog.Duration("interval", interval))
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		trimOnce(ctx, cfg)
		select {
		case <-ctx.Done():
			cfg.Log.Info("daemon stopped")
			return 0
		case <-t.C:
		}
	}
}

// trimOnce runs one trim and logs the result. It reports false when the
// trim failed, had errors, or stopped because ctx was done. A cache locked
// by a different trim is not a failure.
func trimOnce(ctx context.Context, cfg trim.Config) bool {
	log := cfg.Log
	start := time.Now()
	cfg.Now = start
	res, err := trim.Run(ctx, cfg)
	switch {
	case errors.Is(err, trim.ErrBusy):
		log.Info("skipped: a different trim is running")
		return true
	// Run returns an empty Result when ctx is done before the deletes.
	case errors.Is(err, context.Canceled) && res.Scanned == 0:
		log.Info("stopped before scan")
		return false
	case err != nil && !errors.Is(err, context.Canceled):
		log.Error("trim failed", slog.Any("err", err))
		return false
	}
	log.Info("trim done",
		slog.Bool("dry_run", cfg.DryRun),
		slog.Bool("stopped", err != nil),
		slog.Int("scanned", res.Scanned),
		slog.String("scanned_size", formatSize(res.ScannedBytes)),
		slog.Int("age_deleted", res.AgeDeleted),
		slog.String("age_size", formatSize(res.AgeBytes)),
		slog.Int("size_deleted", res.SizeDeleted),
		slog.String("size_size", formatSize(res.SizeBytes)),
		slog.Int("kept_in_use", res.Kept),
		slog.Int("gone", res.Gone),
		slog.String("remaining", formatSize(res.Remaining)),
		slog.Int("errors", res.Errors),
		slog.Duration("took", time.Since(start).Round(time.Millisecond)),
	)
	if res.Shortfall > 0 {
		log.Warn("cache is still over max-size after the trim",
			slog.String("over_by", formatSize(res.Shortfall)),
			slog.String("min_age", formatDuration(cfg.MinAge)),
			slog.Int("kept_in_use", res.Kept),
			slog.Int("errors", res.Errors))
	}
	return err == nil && res.Errors == 0
}
