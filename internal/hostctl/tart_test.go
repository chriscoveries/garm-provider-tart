package hostctl

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// tartStub returns the testdata/bin/tart stub path.
func tartStub(t *testing.T) string {
	t.Helper()
	abs, err := filepath.Abs(filepath.Join("..", "..", "testdata", "bin", "tart"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(abs); err != nil {
		t.Fatalf("tart stub missing: %v", err)
	}
	return abs
}

func TestRealTartCloneUsesStacked(t *testing.T) {
	// Arrange: a RealTart pointed at the stub, with an argv log.
	logPath := filepath.Join(t.TempDir(), "argv.log")
	t.Setenv("TART_STUB_LOG", logPath)
	rt := NewRealTart(tartStub(t))

	// Act: clone.
	if err := rt.Clone("ghcr.io/x/macos@sha256:abc", "garm-abc123"); err != nil {
		t.Fatal(err)
	}

	// Assert: the argv carries --stacked, the image, then the name.
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "clone --stacked ghcr.io/x/macos@sha256:abc garm-abc123") {
		t.Fatalf("clone argv = %q, want stacked clone", string(data))
	}
}

func TestRealTartListParsesLabels(t *testing.T) {
	// Arrange: the stub replays a list with labels.
	logPath := filepath.Join(t.TempDir(), "argv.log")
	t.Setenv("TART_STUB_LOG", logPath)
	t.Setenv("TART_STUB_LIST", `[{"name":"garm-abc123","state":"running","labels":{"garm-controller-id":"controller-1"}}]`)
	rt := NewRealTart(tartStub(t))

	// Act: list.
	vms, err := rt.List()

	// Assert: the VM and its label come back parsed.
	if err != nil {
		t.Fatal(err)
	}
	if len(vms) != 1 {
		t.Fatalf("vms = %d, want 1", len(vms))
	}
	if vms[0].Label("garm-controller-id") != "controller-1" {
		t.Fatalf("label = %q, want controller-1", vms[0].Label("garm-controller-id"))
	}
	logged, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(logged), "list --source local --format json") {
		t.Fatalf("list argv = %q, want only local VMs", logged)
	}
}

func TestRealTartRunUsesNoGraphics(t *testing.T) {
	// Arrange.
	logPath := filepath.Join(t.TempDir(), "argv.log")
	t.Setenv("TART_STUB_LOG", logPath)
	rt := NewRealTart(tartStub(t))

	// Act: run.
	if err := rt.Run("garm-abc123"); err != nil {
		t.Fatal(err)
	}

	// Assert: headless.
	data, _ := os.ReadFile(logPath)
	if !strings.Contains(string(data), "run --no-graphics garm-abc123") {
		t.Fatalf("run argv = %q, want --no-graphics", string(data))
	}
}

func TestRealTartExecBoundedOnHang(t *testing.T) {
	// Arrange: a tart binary that hangs (a guest whose agent never answers),
	// leaving an orphaned child that keeps the stdout/stderr pipe open, and a
	// short context so the kill fires promptly.
	hang := filepath.Join(t.TempDir(), "hang-tart")
	if err := os.WriteFile(hang, []byte("#!/bin/sh\nsleep 300\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	rt := NewRealTart(hang)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	// Act: exec true against the hung binary.
	start := time.Now()
	err := rt.Exec(ctx, "garm-abc123", nil, "true")

	// Assert: it returns in bounded time (probe deadline + a short pipe wait),
	// never blocks forever, and reports an error rather than success.
	if err == nil {
		t.Fatal("Exec error = nil, want a kill/timeout error")
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("Exec took %s, want bounded (<= 5s)", d)
	}
}
