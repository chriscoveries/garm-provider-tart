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

func TestRealTartCloneDefaultsToPortable(t *testing.T) {
	// Arrange: a RealTart pointed at the stub, with an argv log.
	logPath := filepath.Join(t.TempDir(), "argv.log")
	t.Setenv("TART_STUB_LOG", logPath)
	t.Setenv("TART_FLEET_STACKED_CLONES", "")
	rt := NewRealTart(tartStub(t))

	// Act: clone.
	if err := rt.Clone("ghcr.io/x/macos@sha256:abc", "garm-abc123"); err != nil {
		t.Fatal(err)
	}

	// Assert: the argv is compatible with macOS 26 hosts.
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "clone ghcr.io/x/macos@sha256:abc garm-abc123") {
		t.Fatalf("clone argv = %q, want normal clone", string(data))
	}
}

func TestRealTartCloneOptInStacked(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "argv.log")
	t.Setenv("TART_STUB_LOG", logPath)
	t.Setenv("TART_FLEET_STACKED_CLONES", "1")
	rt := NewRealTart(tartStub(t))
	if err := rt.Clone("ghcr.io/x/macos@sha256:abc", "garm-abc123"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "clone --stacked ghcr.io/x/macos@sha256:abc garm-abc123") {
		t.Fatalf("clone argv = %q, want stacked clone when opted in", data)
	}
}

func TestRealTartLabelsUseRegistryOnly(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "argv.log")
	t.Setenv("TART_STUB_LOG", logPath)
	rt := NewRealTart(tartStub(t))
	if err := rt.SetLabels("garm-abc123", map[string]string{controllerLabelKey: "controller-1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(logPath); !os.IsNotExist(err) {
		t.Fatalf("unsupported tart --label command invoked: stat err=%v", err)
	}
}

func TestRealTartSetResourcesUsesDiskSize(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "argv.log")
	t.Setenv("TART_STUB_LOG", logPath)
	t.Setenv("TART_STUB_GET", `{"Disk":"40.000"}`)
	rt := NewRealTart(tartStub(t))
	if err := rt.SetResources("garm-abc123", 4, 8192, 50); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "get garm-abc123 --format json") || !strings.Contains(string(data), "set garm-abc123 --cpu 4 --memory 8192 --disk-size 50") {
		t.Fatalf("resources argv = %q, want Tart disk-size flag", data)
	}
}

func TestRealTartSetResourcesNeverShrinksGoldenImage(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "argv.log")
	t.Setenv("TART_STUB_LOG", logPath)
	t.Setenv("TART_STUB_GET", `{"Disk":100}`)
	rt := NewRealTart(tartStub(t))
	if err := rt.SetResources("garm-abc123", 4, 8192, 50); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "set garm-abc123 --cpu 4 --memory 8192") || strings.Contains(string(data), "--disk-size") {
		t.Fatalf("resources argv = %q, attempted disk shrink", data)
	}
}

func TestRealTartSetResourcesFailsClosedOnUnknownDiskSize(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "argv.log")
	t.Setenv("TART_STUB_LOG", logPath)
	t.Setenv("TART_STUB_GET", `{"Disk":"unknown"}`)
	rt := NewRealTart(tartStub(t))
	if err := rt.SetResources("garm-abc123", 4, 8192, 50); err == nil {
		t.Fatal("accepted an unknown Tart disk size")
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "set garm-abc123") {
		t.Fatalf("set ran with unknown disk size: %q", data)
	}
}

func TestRealTartListParsesLabels(t *testing.T) {
	// Arrange: the stub replays a list with labels.
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
}

func TestRealTartRunDetached(t *testing.T) {
	// Arrange.
	logPath := filepath.Join(t.TempDir(), "argv.log")
	t.Setenv("TART_STUB_LOG", logPath)
	t.Setenv("TART_STUB_RUN_SLEEP", "1")
	rt := NewRealTart(tartStub(t))

	// Act: run.
	start := time.Now()
	if err := rt.Run("garm-abc123"); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("Run blocked for %v on a foreground VM process", elapsed)
	}

	// Assert: headless, and spawned independently of the hostctl process.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(logPath)
		if err == nil && strings.Contains(string(data), "run --no-graphics garm-abc123") {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("detached Tart process did not execute")
}

func TestRealTartExecForwardsStdin(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "argv.log")
	t.Setenv("TART_STUB_LOG", logPath)
	rt := NewRealTart(tartStub(t))
	if err := rt.Exec(context.Background(), "garm-abc123", strings.NewReader("echo ready\n"), "/bin/bash", "-s"); err != nil {
		t.Fatal(err)
	}
	if err := rt.Exec(context.Background(), "garm-abc123", nil, "true"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "exec -i garm-abc123 /bin/bash -s") || !strings.Contains(string(data), "exec garm-abc123 true") {
		t.Fatalf("exec argv = %q, want -i only for forwarded stdin", data)
	}
}

func TestRealHealthFreeDiskBytesUsesMacCompatibleDF(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "df")
	argsFile := filepath.Join(t.TempDir(), "args")
	stub := "#!/bin/sh\nprintf '%s\\n' \"$*\" > \"$DF_TEST_ARGS\"\necho 'Filesystem 1024-blocks Used Available Capacity Mounted on'\necho '/dev/disk1 1000 200 800 20% /'\n"
	if err := os.WriteFile(bin, []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DF_TEST_ARGS", argsFile)
	h := NewRealHealth("", bin, "/tmp")
	available, err := h.FreeDiskBytes()
	if err != nil {
		t.Fatal(err)
	}
	if available != 800*1024 {
		t.Fatalf("free bytes = %d, want %d", available, 800*1024)
	}
	args, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(args)) != "-P -k /tmp" {
		t.Fatalf("df argv = %q, want portable -P -k", args)
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
