package hostctl

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// VM is one entry of `tart list --format json`: the name and raw Tart state
// string ("running"/"stopped"). Labels are optional metadata for injected
// backends and test doubles. Current Tart CLI does not expose VM labels.
type VM struct {
	Name   string            `json:"name"`
	State  string            `json:"state"`
	Labels map[string]string `json:"labels,omitempty"`
}

// Label returns the value of the given label key, or "".
func (v VM) Label(key string) string {
	if v.Labels == nil {
		return ""
	}
	return v.Labels[key]
}

// controllerLabelKey is the metadata key for backends that can supply a
// verifiable controller ID. Real Tart VMs rely on host registry records.
const controllerLabelKey = "garm-controller-id"

// Tart is the subset of the Tart CLI the host driver needs. The concrete
// implementation shells out to the `tart` binary; tests substitute fakes or the
// testdata/bin/tart stub.
type Tart interface {
	Clone(image, name string) error
	SetLabels(name string, labels map[string]string) error
	SetResources(name string, cpu, memoryMB, diskGB int) error
	Run(name string) error
	IP(name string) (string, error)
	Exec(ctx context.Context, name string, stdin io.Reader, args ...string) error
	Stop(name string) error
	Delete(name string) error
	List() ([]VM, error)
	Version() (string, error)
}

// Health probes host facts that Tart itself does not expose.
type Health interface {
	KeychainUnlocked() (bool, error)
	FreeDiskBytes() (uint64, error)
}

// RealTart runs Tart commands via exec.Command. Bin paths are fields so tests
// can point them at the testdata/bin/tart stub.
type RealTart struct {
	TartBin       string
	StackedClones bool
}

// NewRealTart returns a Tart that runs the `tart` binary (or TartBin).
func NewRealTart(tartBin string) *RealTart {
	if tartBin == "" {
		tartBin = "tart"
	}
	// Stacked clones require macOS 27+ and a compatible Tart build. Ordinary
	// clones remain the default for macOS 26 hosts.
	return &RealTart{TartBin: tartBin, StackedClones: os.Getenv("TART_FLEET_STACKED_CLONES") == "1"}
}

func (t *RealTart) run(out io.Writer, args ...string) (string, error) {
	cmd := exec.Command(t.TartBin, args...)
	var stderr bytes.Buffer
	cmd.Stdout = out
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("tart %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return "", nil
}

func (t *RealTart) output(args ...string) (string, error) {
	var stdout bytes.Buffer
	if _, err := t.run(&stdout, args...); err != nil {
		return "", err
	}
	return strings.TrimSpace(stdout.String()), nil
}

// Clone implements Tart.
func (t *RealTart) Clone(image, name string) error {
	args := []string{"clone"}
	if t.StackedClones {
		args = append(args, "--stacked")
	}
	args = append(args, image, name)
	_, err := t.run(io.Discard, args...)
	return err
}

// SetLabels implements Tart. Current Tart CLI releases have no VM label
// setter. The host-owned registry persists the controller token; any VM
// created before its first record was saved must be quarantined on recovery.
// Keep this method for injected backends that support genuine labels.
func (t *RealTart) SetLabels(name string, labels map[string]string) error {
	return nil
}

// SetResources implements Tart.
func (t *RealTart) SetResources(name string, cpu, memoryMB, diskGB int) error {
	args := []string{
		"set", name,
		"--cpu", strconv.Itoa(cpu),
		"--memory", strconv.Itoa(memoryMB),
	}
	if diskGB > 0 {
		currentGB, err := t.diskSize(name)
		if err != nil {
			return err
		}
		// Tart cannot shrink a VM disk. The default Xcode golden image may
		// already be larger than the requested provider default (50 GB).
		if float64(diskGB) > currentGB {
			args = append(args, "--disk-size", strconv.Itoa(diskGB))
		}
	}
	_, err := t.run(io.Discard, args...)
	return err
}

// diskSize reads the current root disk size in GB. Tart's `get` JSON has
// returned Disk as both a number and a quoted number across releases.
func (t *RealTart) diskSize(name string) (float64, error) {
	out, err := t.output("get", name, "--format", "json")
	if err != nil {
		return 0, err
	}
	var config struct {
		Disk json.RawMessage `json:"Disk"`
	}
	if err := json.Unmarshal([]byte(out), &config); err != nil {
		return 0, fmt.Errorf("parse tart get %q: %w", name, err)
	}
	if len(config.Disk) == 0 {
		return 0, fmt.Errorf("tart get %q: disk size is missing", name)
	}
	value := string(config.Disk)
	if config.Disk[0] == '"' {
		if err := json.Unmarshal(config.Disk, &value); err != nil {
			return 0, fmt.Errorf("parse tart get %q disk: %w", name, err)
		}
	}
	size, err := strconv.ParseFloat(value, 64)
	if err != nil || size <= 0 {
		return 0, fmt.Errorf("tart get %q: invalid disk size %q", name, value)
	}
	return size, nil
}

// Run implements Tart.
func (t *RealTart) Run(name string) error {
	// `tart run` stays in the foreground for the entire lifetime of the VM.
	// Detach it from the short-lived hostctl SSH session so VM startup can
	// proceed to the bounded readiness probe and the host flock is released.
	cmd := exec.Command(t.TartBin, "run", "--no-graphics", name)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start tart run %q: %w", name, err)
	}
	return cmd.Process.Release()
}

// IP implements Tart.
func (t *RealTart) IP(name string) (string, error) {
	return t.output("ip", "--wait", "60", name)
}

// Exec implements Tart. The context bounds the `tart exec` process: when ctx
// is done the child is killed, so a guest whose agent never answers cannot pin
// the host flock. Callers that do not need a bound (the bootstrap script runs
// as long as the provider's ssh deadline allows) pass context.Background().
func (t *RealTart) Exec(ctx context.Context, name string, stdin io.Reader, args ...string) error {
	cmdArgs := []string{"exec"}
	if stdin != nil {
		cmdArgs = append(cmdArgs, "-i")
	}
	cmdArgs = append(cmdArgs, name)
	cmdArgs = append(cmdArgs, args...)
	cmd := exec.CommandContext(ctx, t.TartBin, cmdArgs...)
	// WaitDelay bounds the wait for the command's I/O pipes after the context
	// kills the process: a killed `tart exec` can leave orphaned children (the
	// guest-side agent) holding stdout/stderr open, which would otherwise block
	// Wait long after the probe deadline and keep the host flock held.
	cmd.WaitDelay = 1 * time.Second
	var stderr bytes.Buffer
	cmd.Stdin = stdin
	cmd.Stdout = io.Discard
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("tart exec %s: %w: %s", name, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// Stop implements Tart.
func (t *RealTart) Stop(name string) error {
	_, err := t.run(io.Discard, "stop", name)
	return err
}

// Delete implements Tart.
func (t *RealTart) Delete(name string) error {
	_, err := t.run(io.Discard, "delete", name)
	return err
}

// List implements Tart.
func (t *RealTart) List() ([]VM, error) {
	var stdout bytes.Buffer
	if _, err := t.run(&stdout, "list", "--format", "json"); err != nil {
		return nil, err
	}
	var vms []VM
	if err := json.Unmarshal(stdout.Bytes(), &vms); err != nil {
		return nil, fmt.Errorf("parse tart list output: %w", err)
	}
	return vms, nil
}

// Version implements Tart.
func (t *RealTart) Version() (string, error) {
	return t.output("--version")
}

// RealHealth probes the macOS host keychain and free disk.
type RealHealth struct {
	SecurityBin string
	DfBin       string
	Path        string
}

// NewRealHealth returns a Health that runs `security` and `df`.
func NewRealHealth(securityBin, dfBin, path string) *RealHealth {
	if securityBin == "" {
		securityBin = "security"
	}
	if dfBin == "" {
		dfBin = "df"
	}
	if path == "" {
		path = "/var/db/tart-fleet"
	}
	return &RealHealth{SecurityBin: securityBin, DfBin: dfBin, Path: path}
}

// KeychainUnlocked implements Health. `security show-keychain-info` exits 0
// when the login keychain is unlocked and non-zero when it is locked, which is
// the failure macOS 15 surfaces as SecKeyCreateRandomEntry_ios during VM start.
func (h *RealHealth) KeychainUnlocked() (bool, error) {
	cmd := exec.Command(h.SecurityBin, "show-keychain-info")
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		return false, nil
	}
	return true, nil
}

// FreeDiskBytes implements Health using POSIX `df -P -k <path>`, whose
// available-space column is measured in 1024-byte blocks on macOS and Linux.
func (h *RealHealth) FreeDiskBytes() (uint64, error) {
	out, err := exec.Command(h.DfBin, "-P", "-k", h.Path).Output()
	if err != nil {
		return 0, fmt.Errorf("df %s: %w", h.Path, err)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) < 2 {
		return 0, fmt.Errorf("unexpected df output: %q", strings.TrimSpace(string(out)))
	}
	// The last line is the data line for the filesystem containing h.Path;
	// field 3 is the available count of 1024-byte blocks.
	fields := strings.Fields(lines[len(lines)-1])
	if len(fields) < 4 {
		return 0, fmt.Errorf("unexpected df output: %q", strings.TrimSpace(string(out)))
	}
	available, err := strconv.ParseUint(fields[3], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse df output: %w", err)
	}
	if available > ^uint64(0)/1024 {
		return 0, fmt.Errorf("df available blocks overflow bytes: %d", available)
	}
	return available * 1024, nil
}
