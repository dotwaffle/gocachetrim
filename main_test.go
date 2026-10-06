package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseDuration(t *testing.T) {
	tests := []struct {
		in      string
		want    time.Duration
		wantErr bool
	}{
		{in: "3d", want: 3 * day},
		{in: "1.5d", want: 36 * time.Hour},
		{in: "0d", want: 0},
		{in: "2h", want: 2 * time.Hour},
		{in: "90m", want: 90 * time.Minute},
		{in: "0", want: 0},
		{in: "d", wantErr: true},
		{in: "-1d", wantErr: true},
		{in: "-1h", wantErr: true},
		{in: "1w", wantErr: true},
		{in: "1e10d", wantErr: true},
		{in: "106751d", want: 106751 * day},
		{in: "106752d", wantErr: true},
		{in: "NaNd", wantErr: true},
	}
	for _, tt := range tests {
		got, err := parseDuration(tt.in)
		if (err != nil) != tt.wantErr || got != tt.want {
			t.Errorf("parseDuration(%q) = %v, %v; want %v, error %v", tt.in, got, err, tt.want, tt.wantErr)
		}
	}
}

func TestParseSize(t *testing.T) {
	tests := []struct {
		in      string
		want    int64
		wantErr bool
	}{
		{in: "0", want: 0},
		{in: "512", want: 512},
		{in: "512B", want: 512},
		{in: "10G", want: 10 << 30},
		{in: "10g", want: 10 << 30},
		{in: "10GB", want: 10 << 30},
		{in: "10GiB", want: 10 << 30},
		{in: "1.5K", want: 1536},
		{in: "2T", want: 2 << 40},
		{in: "", wantErr: true},
		{in: "G", wantErr: true},
		{in: "-1G", wantErr: true},
		{in: "10X", wantErr: true},
		{in: "1e30T", wantErr: true},
		{in: "8388607T", want: 8388607 << 40},
		{in: "8388608T", wantErr: true},
		{in: "9223372036854775808", wantErr: true},
	}
	for _, tt := range tests {
		got, err := parseSize(tt.in)
		if (err != nil) != tt.wantErr || got != tt.want {
			t.Errorf("parseSize(%q) = %v, %v; want %v, error %v", tt.in, got, err, tt.want, tt.wantErr)
		}
	}
}

func TestFormat(t *testing.T) {
	sizes := map[int64]string{0: "0B", 1023: "1023B", 1536: "1.5KiB", 10 << 30: "10.0GiB", 3 << 50: "3072.0TiB"}
	for n, want := range sizes {
		if got := formatSize(n); got != want {
			t.Errorf("formatSize(%d) = %q, want %q", n, got, want)
		}
	}
	durations := map[time.Duration]string{0: "0s", 3 * day: "3d", 2 * time.Hour: "2h0m0s", 36 * time.Hour: "36h0m0s"}
	for d, want := range durations {
		if got := formatDuration(d); got != want {
			t.Errorf("formatDuration(%v) = %q, want %q", d, got, want)
		}
	}
}

// newCache makes a minimal cache directory that trim.Run accepts.
func newCache(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	readme := "This directory holds cached build artifacts from the Go build system.\n"
	if err := os.WriteFile(filepath.Join(dir, "README"), []byte(readme), 0o666); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestRunExitStatus(t *testing.T) {
	cache := newCache(t)
	tests := []struct {
		name     string
		args     []string
		want     int
		wantText string
	}{
		{name: "help", args: []string{"-h"}, want: 0, wantText: "usage: gocachetrim"},
		{name: "unknown flag", args: []string{"-nope"}, want: 2},
		{name: "bad size", args: []string{"-max-size", "10X"}, want: 2, wantText: "invalid size"},
		{name: "positional", args: []string{"-cache", cache, "extra"}, want: 2, wantText: "unexpected arguments"},
		{name: "nothing to do", args: []string{"-max-age", "0", "-max-size", "0"}, want: 2, wantText: "nothing to do"},
		{name: "max age below min age", args: []string{"-max-age", "1h"}, want: 2, wantText: "less than -min-age"},
		{name: "bad metric", args: []string{"-size-metric", "disk"}, want: 2, wantText: "-size-metric"},
		{name: "no workers", args: []string{"-workers", "0"}, want: 2, wantText: "-workers"},
		{name: "zero interval", args: []string{"-daemon", "-interval", "0"}, want: 2, wantText: "-interval"},
		{name: "not a cache", args: []string{"-cache", t.TempDir()}, want: 1, wantText: "not a Go build cache"},
		{name: "ok", args: []string{"-cache", cache}, want: 0, wantText: "trim done"},
		{name: "low min age warns", args: []string{"-cache", cache, "-min-age", "10m"}, want: 0, wantText: "min-age is less than 1h"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stderr bytes.Buffer
			got := run(t.Context(), tt.args, &stderr)
			if got != tt.want {
				t.Errorf("exit status = %d, want %d; stderr:\n%s", got, tt.want, &stderr)
			}
			if !strings.Contains(stderr.String(), tt.wantText) {
				t.Errorf("stderr does not contain %q:\n%s", tt.wantText, &stderr)
			}
		})
	}
}
