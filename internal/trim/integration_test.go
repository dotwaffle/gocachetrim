package trim

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// TestGoBuildAfterTrim trims a cache that the go command made, and makes
// sure that the go command can use the cache after the trim.
func TestGoBuildAfterTrim(t *testing.T) {
	if testing.Short() {
		t.Skip("builds with the go command")
	}
	goBin, lookErr := exec.LookPath("go")
	if lookErr != nil {
		t.Skip("no go command:", lookErr)
	}
	cache := t.TempDir()
	mod := t.TempDir()
	files := map[string]string{
		"go.mod":  "module example.com/hello\n\ngo 1.25\n",
		"main.go": "package main\n\nimport \"fmt\"\n\nfunc main() { fmt.Println(\"hello\") }\n",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(mod, name), []byte(body), 0o666); err != nil {
			t.Fatal(err)
		}
	}
	build := func() {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), goBin, "build", "-buildvcs=false", "-o", filepath.Join(t.TempDir(), "hello"), ".")
		cmd.Dir = mod
		cmd.Env = append(os.Environ(), "GOCACHE="+cache, "GOCACHEPROG=", "GOFLAGS=", "GOTOOLCHAIN=local", "GOPROXY=off", "CGO_ENABLED=0")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("go build: %v\n%s", err, out)
		}
	}
	build()

	// All entries are new, so MinAge keeps them, and the size limit
	// cannot be met.
	cfg := Config{Dir: cache, MaxSize: 1, MinAge: time.Hour, Metric: Allocated, Workers: 8, Now: time.Now()}
	res, err := Run(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if res.Scanned == 0 || res.SizeDeleted != 0 || res.Shortfall == 0 || res.Errors != 0 {
		t.Errorf("size trim = %+v, want entries scanned and kept with a shortfall", res)
	}

	// Two days later, all entries are older than MaxAge.
	cfg = Config{Dir: cache, MaxAge: 24 * time.Hour, Metric: Allocated, Workers: 8, Now: time.Now().Add(48 * time.Hour)}
	res, err = Run(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if res.Scanned == 0 || res.AgeDeleted != res.Scanned || res.Remaining != 0 || res.Errors != 0 {
		t.Errorf("age trim = %+v, want all entries deleted", res)
	}
	b, err := os.ReadFile(filepath.Join(cache, "trim.txt"))
	if err != nil || string(b) != strconv.FormatInt(cfg.Now.Unix(), 10) {
		t.Errorf("trim.txt = %q, %v; want %d", b, err, cfg.Now.Unix())
	}

	build()
}
