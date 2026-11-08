package hostctl

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeTart is an in-memory Tart for precise control over each verb's outcome.
type fakeTart struct {
	mu                 sync.Mutex
	vms                map[string]VM
	cloneErr           error
	runErr             error
	execErr            error
	execHang           bool          // when true, Exec blocks until ctx is done
	execEntered        chan struct{} // when set, closed when Exec is entered
	deleteErr          error
	deleteNoop         bool // when true, Delete returns nil without removing the VM
	listErr            error
	listErrAfterDelete error
	version            string
	ip                 string
	clones             []string
}

func newFakeTart() *fakeTart {
	return &fakeTart{vms: map[string]VM{}, version: "tart 2.0.0", ip: "192.168.64.5"}
}

func (f *fakeTart) Clone(image, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.clones = append(f.clones, name)
	if f.cloneErr != nil {
		return f.cloneErr
	}
	f.vms[name] = VM{Name: name, State: "stopped"}
	return nil
}

func (f *fakeTart) SetLabels(name string, labels map[string]string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	vm := f.vms[name]
	if vm.Labels == nil {
		vm.Labels = map[string]string{}
	}
	for k, v := range labels {
		vm.Labels[k] = v
	}
	f.vms[name] = vm
	return nil
}

func (f *fakeTart) SetResources(name string, cpu, memoryMB, diskGB int) error { return nil }
func (f *fakeTart) Run(name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.runErr != nil {
		return f.runErr
	}
	vm := f.vms[name]
	vm.State = "running"
	f.vms[name] = vm
	return nil
}
func (f *fakeTart) IP(name string) (string, error) { return f.ip, nil }
func (f *fakeTart) Exec(ctx context.Context, name string, stdin io.Reader, args ...string) error {
	if f.execEntered != nil {
		select {
		case <-f.execEntered:
		default:
			close(f.execEntered)
		}
	}
	if f.execHang {
		<-ctx.Done()
		return ctx.Err()
	}
	return f.execErr
}
func (f *fakeTart) Stop(name string) error { return nil }
func (f *fakeTart) Delete(name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.deleteErr != nil {
		return f.deleteErr
	}
	if !f.deleteNoop {
		delete(f.vms, name)
	}
	if f.listErrAfterDelete != nil {
		f.listErr = f.listErrAfterDelete
	}
	return nil
}
func (f *fakeTart) List() ([]VM, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	out := make([]VM, 0, len(f.vms))
	for _, vm := range f.vms {
		out = append(out, vm)
	}
	return out, nil
}
func (f *fakeTart) Version() (string, error) { return f.version, nil }

type fakeHealth struct{ unlocked bool }

func (h fakeHealth) KeychainUnlocked() (bool, error) { return h.unlocked, nil }
func (h fakeHealth) FreeDiskBytes() (uint64, error)  { return 100 << 30, nil }

func newTestService(t *testing.T, tart *fakeTart) *Service {
	t.Helper()
	return newTestServiceWithHealth(t, tart, fakeHealth{unlocked: true})
}

func newTestServiceWithHealth(t *testing.T, tart *fakeTart, health Health) *Service {
	t.Helper()
	svc, err := NewService(Options{
		Root:         t.TempDir(),
		Tart:         tart,
		Health:       health,
		HostName:     "mac-mini-01",
		ControllerID: "controller-1",
		SlotsPerHost: 2,
	})
	if err != nil {
		t.Fatalf("NewService error = %v, want nil", err)
	}
	return svc
}

func createReq(name string) CreateRequest {
	return CreateRequest{
		Name:       name,
		Image:      "ghcr.io/cirruslabs/macos-tahoe-xcode@sha256:abc",
		CPU:        4,
		MemoryMB:   8192,
		DiskGB:     50,
		Controller: "controller-1",
		Owner:      "owner-1",
		Host:       "mac-mini-01",
	}
}

func faultOf(t *testing.T, err error) *FaultError {
	t.Helper()
	var fe *FaultError
	if !errors.As(err, &fe) {
		t.Fatalf("error %v is not a FaultError", err)
	}
	return fe
}

func TestCreateReachesReady(t *testing.T) {
	// Arrange: a healthy host.
	tart := newFakeTart()
	svc := newTestService(t, tart)

	// Act: create.
	inst, err := svc.Create(createReq("garm-abc123"))

	// Assert: ready, with the provider ID and IP.
	if err != nil {
		t.Fatalf("Create error = %v, want nil", err)
	}
	if inst.State != StateReady {
		t.Fatalf("state = %s, want ready", inst.State)
	}
	if inst.ProviderID != "mac-mini-01/garm-abc123" {
		t.Fatalf("provider_id = %q, want mac-mini-01/garm-abc123", inst.ProviderID)
	}
	if inst.IP != "192.168.64.5" {
		t.Fatalf("ip = %q, want 192.168.64.5", inst.IP)
	}
}

func TestCreateExecFailureStaysRunning(t *testing.T) {
	// Arrange: tart exec (the readiness probe) fails.
	tart := newFakeTart()
	tart.execErr = fmt.Errorf("guest agent not up")
	svc := newTestService(t, tart)

	// Act: create.
	inst, err := svc.Create(createReq("garm-abc123"))

	// Assert: no error, but the record is running, never ready, with last_error.
	if err != nil {
		t.Fatalf("Create error = %v, want nil (caller decides)", err)
	}
	if inst.State != StateRunning {
		t.Fatalf("state = %s, want running", inst.State)
	}
	if inst.LastError == "" {
		t.Fatal("last_error is empty, want the exec failure")
	}
	if rec, _ := svc.reg.Load("garm-abc123"); rec.State != StateRunning {
		t.Fatalf("persisted state = %s, want running", rec.State)
	}
}

func TestCreateHungProbeReleasesFlock(t *testing.T) {
	// Arrange: a tart whose exec blocks until ctx is done, a short probe
	// timeout so the hang is bounded in the test, and a signal so the test
	// knows the probe is under way (and the exclusive flock definitely held).
	tart := newFakeTart()
	tart.execHang = true
	tart.execEntered = make(chan struct{})
	svc := newTestService(t, tart)
	svc.probeTimeout = 100 * time.Millisecond

	// Act: create in the background; it holds the exclusive flock through the
	// hung readiness probe until the host-side timeout fires.
	createDone := make(chan struct{})
	var createErr error
	go func() {
		_, createErr = svc.Create(createReq("garm-hang01"))
		close(createDone)
	}()
	<-tart.execEntered // the probe is hung and the flock is held

	// A concurrent get of another VM must complete in bounded time: it blocks
	// only until the probe times out and releases the exclusive flock.
	getDone := make(chan struct{})
	go func() {
		_, _ = svc.Get("garm-other")
		close(getDone)
	}()
	select {
	case <-getDone:
	case <-time.After(2 * time.Second):
		t.Fatal("concurrent get blocked: the hung readiness probe held the flock")
	}

	// Assert: create returned in bounded time, the record is running (never
	// ready) with last_error, so the instance fails closed.
	select {
	case <-createDone:
		if createErr != nil {
			t.Fatalf("Create error = %v, want nil (caller decides)", createErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("create did not return after the probe timeout")
	}
	rec, err := svc.reg.Load("garm-hang01")
	if err != nil {
		t.Fatalf("record missing after create: %v", err)
	}
	if rec.State != StateRunning {
		t.Fatalf("state = %s, want running", rec.State)
	}
	if rec.LastError == "" {
		t.Fatal("last_error is empty, want the probe timeout error")
	}
}

func TestCreateKeychainLockedRefuses(t *testing.T) {
	// Arrange: a locked login keychain.
	tart := newFakeTart()
	svc := newTestServiceWithHealth(t, tart, fakeHealth{unlocked: false})

	// Act: create.
	_, err := svc.Create(createReq("garm-abc123"))

	// Assert: a keychain fault naming the fix, and no Tart command ran.
	fe := faultOf(t, err)
	if fe.Fault != FaultKeychain {
		t.Fatalf("fault = %q, want %q", fe.Fault, FaultKeychain)
	}
	if !strings.Contains(fe.Message, "unlock-keychain") {
		t.Fatalf("message %q does not name the fix", fe.Message)
	}
	if len(tart.clones) != 0 {
		t.Fatalf("clone ran %d times, want 0", len(tart.clones))
	}
}

func TestCreateDrainedRefuses(t *testing.T) {
	// Arrange: a drained host.
	tart := newFakeTart()
	svc := newTestService(t, tart)
	if err := svc.Drain(); err != nil {
		t.Fatal(err)
	}

	// Act: create.
	_, err := svc.Create(createReq("garm-abc123"))

	// Assert: a drain fault and no clone.
	if fe := faultOf(t, err); fe.Fault != FaultDrained {
		t.Fatalf("fault = %q, want %q", fe.Fault, FaultDrained)
	}
	if len(tart.clones) != 0 {
		t.Fatalf("clone ran %d times, want 0", len(tart.clones))
	}
}

func TestCreateCapacityRefusesWithoutResidue(t *testing.T) {
	// Arrange: two slots.
	tart := newFakeTart()
	svc := newTestService(t, tart)

	// Act: fill both slots, then attempt a third.
	for _, name := range []string{"garm-a00001", "garm-b00002"} {
		if _, err := svc.Create(createReq(name)); err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
	}
	_, err := svc.Create(createReq("garm-c00003"))

	// Assert: a capacity fault, and no third clone or record.
	if fe := faultOf(t, err); fe.Fault != FaultCapacity {
		t.Fatalf("fault = %q, want %q", fe.Fault, FaultCapacity)
	}
	if len(tart.clones) != 2 {
		t.Fatalf("clones = %d, want 2", len(tart.clones))
	}
	if _, lerr := svc.reg.Load("garm-c00003"); lerr != ErrNotFound {
		t.Fatalf("third name left a record (err=%v)", lerr)
	}
}

func TestConcurrentCreateNeverExceedsCapacity(t *testing.T) {
	// Arrange: one host, two slots, ten concurrent creates.
	tart := newFakeTart()
	svc := newTestService(t, tart)

	const n = 10
	var wg sync.WaitGroup
	errs := make([]error, n)
	names := make([]string, n)
	for i := 0; i < n; i++ {
		names[i] = fmt.Sprintf("garm-w%05d", i)
	}
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = svc.Create(createReq(names[i]))
		}(i)
	}
	wg.Wait()

	// Assert: exactly two succeed, the rest are capacity faults, and the
	// registry holds exactly two live records (no lost or double-issued slot).
	var ok, capacity, other int
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case faultOf(t, err).Fault == FaultCapacity:
			capacity++
		default:
			other++
		}
	}
	if ok != 2 {
		t.Fatalf("successes = %d, want 2", ok)
	}
	if capacity != n-2 {
		t.Fatalf("capacity faults = %d, want %d", capacity, n-2)
	}
	if other != 0 {
		t.Fatalf("unexpected error type in %d creates", other)
	}
	records, err := svc.reg.All()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 {
		t.Fatalf("live records = %d, want 2", len(records))
	}
}

func TestCrashBetweenCloneAndRecordWriteAdoptsOrQuarantines(t *testing.T) {
	// Arrange: a VM present in `tart list` with no registry record — exactly
	// what a provider killed between `tart clone` and the record write leaves.
	tart := newFakeTart()
	svc := newTestService(t, tart)
	tart.mu.Lock()
	tart.vms["garm-orphan1"] = VM{Name: "garm-orphan1", Labels: map[string]string{controllerLabelKey: "controller-1"}}
	tart.vms["garm-orphan2"] = VM{Name: "garm-orphan2"}
	tart.mu.Unlock()

	// Act: reconcile.
	if err := svc.Reconcile(); err != nil {
		t.Fatal(err)
	}

	// Assert: the labelled VM is adopted, the unlabelled VM is quarantined.
	adopted, err := svc.reg.Load("garm-orphan1")
	if err != nil {
		t.Fatalf("labelled VM not adopted: %v", err)
	}
	if adopted.State != StateAdopted {
		t.Fatalf("adopted state = %s, want adopted", adopted.State)
	}
	quarantined, err := svc.reg.Load("garm-orphan2")
	if err != nil {
		t.Fatalf("unlabelled VM not quarantined: %v", err)
	}
	if quarantined.State != StateQuarantined {
		t.Fatalf("quarantined state = %s, want quarantined", quarantined.State)
	}
}

func TestAdoptForeignControllerLabelQuarantined(t *testing.T) {
	// Arrange: a VM carrying another controller's label.
	tart := newFakeTart()
	svc := newTestService(t, tart)
	tart.mu.Lock()
	tart.vms["garm-foreign"] = VM{Name: "garm-foreign", Labels: map[string]string{controllerLabelKey: "other-controller"}}
	tart.mu.Unlock()

	// Act: reconcile.
	if err := svc.Reconcile(); err != nil {
		t.Fatal(err)
	}

	// Assert: the VM occupies a slot but cannot be adopted or automatically deleted.
	rec, err := svc.reg.Load("garm-foreign")
	if err != nil {
		t.Fatalf("foreign VM not quarantined: %v", err)
	}
	if rec.State != StateQuarantined {
		t.Fatalf("foreign state = %s, want quarantined", rec.State)
	}
	if _, exists := tart.vms["garm-foreign"]; !exists {
		t.Fatal("foreign VM was deleted")
	}
}

func TestAdoptCountsAgainstCapacity(t *testing.T) {
	// Arrange: one adopted VM occupying a slot.
	tart := newFakeTart()
	svc := newTestService(t, tart)
	tart.mu.Lock()
	tart.vms["garm-adopted"] = VM{Name: "garm-adopted", Labels: map[string]string{controllerLabelKey: "controller-1"}}
	tart.mu.Unlock()
	if err := svc.Reconcile(); err != nil {
		t.Fatal(err)
	}

	// Act: one create fills the remaining slot; a second must fault.
	if _, err := svc.Create(createReq("garm-a00001")); err != nil {
		t.Fatalf("first create: %v", err)
	}
	_, err := svc.Create(createReq("garm-b00002"))

	// Assert: the second create hits capacity because adopted + used == 2.
	if fe := faultOf(t, err); fe.Fault != FaultCapacity {
		t.Fatalf("fault = %q, want %q", fe.Fault, FaultCapacity)
	}
}

func TestQuarantineUnlabelledVMPreservedAfterTTL(t *testing.T) {
	// Arrange: an unlabelled VM quarantined, and a clock we control.
	tart := newFakeTart()
	svc := newTestService(t, tart)
	now := time.Now()
	svc.now = func() time.Time { return now }
	tart.mu.Lock()
	tart.vms["garm-stray"] = VM{Name: "garm-stray"}
	tart.mu.Unlock()
	if err := svc.Reconcile(); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.reg.Load("garm-stray"); err != nil {
		t.Fatalf("stray VM not quarantined: %v", err)
	}

	// Act: advance past the historical 24 h TTL and reconcile again.
	now = now.Add(25 * time.Hour)
	if err := svc.Reconcile(); err != nil {
		t.Fatal(err)
	}

	// Assert: without verified ownership, the VM and its registry record stay.
	if rec, err := svc.reg.Load("garm-stray"); err != nil || rec.State != StateQuarantined {
		t.Fatalf("stray VM not quarantined after TTL: rec=%+v err=%v", rec, err)
	}
	tart.mu.Lock()
	_, present := tart.vms["garm-stray"]
	tart.mu.Unlock()
	if !present {
		t.Fatal("stray VM deleted after TTL")
	}
}

func TestQuarantineConsumesCapacity(t *testing.T) {
	tart := newFakeTart()
	svc := newTestService(t, tart)
	tart.vms["untracked-1"] = VM{Name: "untracked-1"}
	tart.vms["untracked-2"] = VM{Name: "untracked-2"}

	if _, err := svc.Create(createReq("garm-abc123")); faultOf(t, err).Fault != FaultCapacity {
		t.Fatalf("create fault = %v, want capacity", err)
	}
	if len(tart.clones) != 0 {
		t.Fatalf("cloned %d VMs on a full host", len(tart.clones))
	}
}

func TestDeleteRefusesUnknownAndQuarantinedVMs(t *testing.T) {
	tart := newFakeTart()
	svc := newTestService(t, tart)
	tart.vms["untracked-1"] = VM{Name: "untracked-1"}

	// An untracked VM is protected even before any reconciliation.
	if fe := faultOf(t, svc.Delete("untracked-1")); fe.Fault != FaultValidation {
		t.Fatalf("untracked delete fault = %q, want validation", fe.Fault)
	}
	if err := svc.Reconcile(); err != nil {
		t.Fatal(err)
	}
	if fe := faultOf(t, svc.Delete("untracked-1")); fe.Fault != FaultValidation {
		t.Fatalf("quarantined delete fault = %q, want validation", fe.Fault)
	}
	if _, exists := tart.vms["untracked-1"]; !exists {
		t.Fatal("untracked VM was deleted")
	}
}

func TestDeleteListFailureKeepsTombstone(t *testing.T) {
	tart := newFakeTart()
	svc := newTestService(t, tart)
	if _, err := svc.Create(createReq("garm-abc123")); err != nil {
		t.Fatal(err)
	}
	tart.listErrAfterDelete = errors.New("tart list temporarily unavailable")
	if err := svc.Delete("garm-abc123"); err == nil {
		t.Fatal("delete succeeded without proving VM absence")
	}
	rec, err := svc.reg.Load("garm-abc123")
	if err != nil || rec.State != StateDeleting {
		t.Fatalf("tombstone lost after list failure: rec=%+v err=%v", rec, err)
	}
	tart.listErr = nil
	tart.listErrAfterDelete = nil
	if err := svc.Delete("garm-abc123"); err != nil {
		t.Fatalf("delete retry failed: %v", err)
	}
}

func TestDeleteListFailureBeforeDeletePreservesVM(t *testing.T) {
	tart := newFakeTart()
	svc := newTestService(t, tart)
	if _, err := svc.Create(createReq("garm-abc123")); err != nil {
		t.Fatal(err)
	}
	tart.listErr = errors.New("tart list temporarily unavailable")
	if err := svc.Delete("garm-abc123"); err == nil {
		t.Fatal("delete succeeded without being able to inspect Tart")
	}
	if _, exists := tart.vms["garm-abc123"]; !exists {
		t.Fatal("delete ran when Tart list was unavailable")
	}
}

func TestEmptyControllerCannotAdoptUnlabelledVM(t *testing.T) {
	tart := newFakeTart()
	svc := newTestService(t, tart)
	svc.controllerID = ""
	tart.vms["garm-unlabelled"] = VM{Name: "garm-unlabelled"}
	if err := svc.Reconcile(); err != nil {
		t.Fatal(err)
	}
	rec, err := svc.reg.Load("garm-unlabelled")
	if err != nil || rec.State != StateQuarantined {
		t.Fatalf("unlabelled VM wrongly adopted: rec=%+v err=%v", rec, err)
	}
}

func TestDeleteProvesRelease(t *testing.T) {
	// Arrange: a ready VM.
	tart := newFakeTart()
	svc := newTestService(t, tart)
	if _, err := svc.Create(createReq("garm-abc123")); err != nil {
		t.Fatal(err)
	}

	// Act: delete.
	if err := svc.Delete("garm-abc123"); err != nil {
		t.Fatalf("Delete error = %v, want nil", err)
	}

	// Assert: the record is gone and the VM is gone from tart list.
	if _, err := svc.reg.Load("garm-abc123"); err != ErrNotFound {
		t.Fatalf("record still present (err=%v)", err)
	}
	tart.mu.Lock()
	_, present := tart.vms["garm-abc123"]
	tart.mu.Unlock()
	if present {
		t.Fatal("VM still present in tart list after delete")
	}
}

func TestDeleteTartFailureKeepsTombstoneThenRecovers(t *testing.T) {
	// Arrange: a ready VM, then Tart delete starts failing.
	tart := newFakeTart()
	svc := newTestService(t, tart)
	if _, err := svc.Create(createReq("garm-abc123")); err != nil {
		t.Fatal(err)
	}
	tart.deleteErr = fmt.Errorf("tart delete: device busy")

	// Act: first delete fails.
	err := svc.Delete("garm-abc123")

	// Assert: non-nil error, tombstone kept, slot still counted.
	if err == nil {
		t.Fatal("Delete succeeded despite tart failure, want error")
	}
	rec, lerr := svc.reg.Load("garm-abc123")
	if lerr != nil {
		t.Fatalf("tombstone lost: %v", lerr)
	}
	if rec.State != StateDeleting {
		t.Fatalf("state = %s, want deleting", rec.State)
	}
	used, _, _, _ := svc.capacity()
	if used != 1 {
		t.Fatalf("used = %d, want 1 (slot still counted)", used)
	}

	// Act: Tart recovers, the next delete completes.
	tart.deleteErr = nil
	if err := svc.Delete("garm-abc123"); err != nil {
		t.Fatalf("second Delete error = %v, want nil", err)
	}

	// Assert: record removed and the slot freed.
	if _, lerr := svc.reg.Load("garm-abc123"); lerr != ErrNotFound {
		t.Fatalf("record still present (err=%v)", lerr)
	}
	used, _, _, _ = svc.capacity()
	if used != 0 {
		t.Fatalf("used = %d, want 0 (slot freed)", used)
	}
}

func TestDeleteLieKeepsSlotHeld(t *testing.T) {
	// Arrange: tart delete returns success but leaves the VM in the list.
	tart := newFakeTart()
	tart.deleteNoop = true
	svc := newTestService(t, tart)
	if _, err := svc.Create(createReq("garm-abc123")); err != nil {
		t.Fatal(err)
	}

	// Act: delete.
	err := svc.Delete("garm-abc123")

	// Assert: error, tombstone kept, slot held.
	if err == nil {
		t.Fatal("Delete reported success although the VM is still listed")
	}
	rec, lerr := svc.reg.Load("garm-abc123")
	if lerr != nil {
		t.Fatalf("tombstone lost: %v", lerr)
	}
	if rec.State != StateDeleting {
		t.Fatalf("state = %s, want deleting", rec.State)
	}
	used, _, _, _ := svc.capacity()
	if used != 1 {
		t.Fatalf("used = %d, want 1", used)
	}
}

func TestDeleteAbsentVMIsNoOp(t *testing.T) {
	// Arrange: no record, no VM.
	tart := newFakeTart()
	svc := newTestService(t, tart)

	// Act / Assert: deleting an unknown name succeeds silently.
	if err := svc.Delete("garm-gone00"); err != nil {
		t.Fatalf("Delete error = %v, want nil", err)
	}
}

func TestBootstrapReadyToIdle(t *testing.T) {
	// Arrange: a ready VM.
	tart := newFakeTart()
	svc := newTestService(t, tart)
	if _, err := svc.Create(createReq("garm-abc123")); err != nil {
		t.Fatal(err)
	}

	// Act: bootstrap with a script.
	inst, err := svc.Bootstrap("garm-abc123", strings.NewReader("#!/bin/bash\necho hi\n"))

	// Assert: idle.
	if err != nil {
		t.Fatalf("Bootstrap error = %v, want nil", err)
	}
	if inst.State != StateIdle {
		t.Fatalf("state = %s, want idle", inst.State)
	}
}

func TestDrainThenUndrain(t *testing.T) {
	// Arrange: a host.
	tart := newFakeTart()
	svc := newTestService(t, tart)

	// Act: drain, then create fails, then undrain, then create works.
	if err := svc.Drain(); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Create(createReq("garm-a00001")); faultOf(t, err).Fault != FaultDrained {
		t.Fatalf("create while drained did not fault drained")
	}
	if err := svc.Undrain(); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Create(createReq("garm-a00001")); err != nil {
		t.Fatalf("create after undrain: %v", err)
	}
}

func TestListExcludesQuarantined(t *testing.T) {
	// Arrange: one created, one adopted, one quarantined VM.
	tart := newFakeTart()
	svc := newTestService(t, tart)
	if _, err := svc.Create(createReq("garm-a00001")); err != nil {
		t.Fatal(err)
	}
	tart.mu.Lock()
	tart.vms["garm-adopted"] = VM{Name: "garm-adopted", Labels: map[string]string{controllerLabelKey: "controller-1"}}
	tart.vms["garm-stray"] = VM{Name: "garm-stray"}
	tart.mu.Unlock()
	if err := svc.Reconcile(); err != nil {
		t.Fatal(err)
	}

	// Act: list.
	instances, err := svc.List("", "")

	// Assert: created + adopted reported, quarantined excluded.
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, inst := range instances {
		names[inst.Name] = true
	}
	if !names["garm-a00001"] || !names["garm-adopted"] {
		t.Fatalf("list = %v, want created and adopted", names)
	}
	if names["garm-stray"] {
		t.Fatalf("quarantined VM leaked into list")
	}
}

func TestStatusReportsHealthGate(t *testing.T) {
	// Arrange: one created VM.
	tart := newFakeTart()
	svc := newTestService(t, tart)
	if _, err := svc.Create(createReq("garm-abc123")); err != nil {
		t.Fatal(err)
	}

	// Act: status.
	st, err := svc.Status()

	// Assert: version, keychain, capacity and per-VM state are all reported.
	if err != nil {
		t.Fatal(err)
	}
	if st.TartVersion == "" {
		t.Fatal("tart_version is empty")
	}
	if !st.KeychainUnlocked {
		t.Fatal("keychain_unlocked = false, want true")
	}
	if st.Capacity.Total != 2 || st.Capacity.Used != 1 {
		t.Fatalf("capacity = %+v, want total=2 used=1", st.Capacity)
	}
	if len(st.Instances) != 1 {
		t.Fatalf("instances = %d, want 1", len(st.Instances))
	}
}

func TestNewServiceRejectsSlotsAboveLicenceCap(t *testing.T) {
	// Arrange: a slot count above the macOS licence cap.
	tart := newFakeTart()

	// Act: construct a service with SlotsPerHost = 5.
	svc, err := NewService(Options{
		Root:         t.TempDir(),
		Tart:         tart,
		Health:       fakeHealth{unlocked: true},
		HostName:     "mac-mini-01",
		ControllerID: "controller-1",
		SlotsPerHost: 5,
	})

	// Assert: rejected with a validation fault and no service, so capacity can
	// never exceed the cap.
	if err == nil {
		t.Fatal("NewService accepted SlotsPerHost = 5, want rejection")
	}
	if svc != nil {
		t.Fatal("NewService returned a service alongside the error, want nil")
	}
	if fe := faultOf(t, err); fe.Fault != FaultValidation {
		t.Fatalf("fault = %q, want %q", fe.Fault, FaultValidation)
	}
}

func TestNewServiceDefaultsSlotsToLicenceCap(t *testing.T) {
	// Arrange: no explicit slot count.
	tart := newFakeTart()

	// Act: construct a service with SlotsPerHost = 0 (the default).
	svc, err := NewService(Options{
		Root:         t.TempDir(),
		Tart:         tart,
		Health:       fakeHealth{unlocked: true},
		HostName:     "mac-mini-01",
		ControllerID: "controller-1",
	})
	if err != nil {
		t.Fatalf("NewService error = %v, want nil", err)
	}
	st, err := svc.Status()
	if err != nil {
		t.Fatalf("Status error = %v, want nil", err)
	}

	// Assert: the default is the licence cap, never more.
	if st.Capacity.Total != MaxSlotsPerHost {
		t.Fatalf("capacity.total = %d, want %d", st.Capacity.Total, MaxSlotsPerHost)
	}
}

func TestCreateInvalidNameRejectedBeforeTart(t *testing.T) {
	// Arrange: a name that could reach a shell.
	tart := newFakeTart()
	svc := newTestService(t, tart)

	// Act: create with an invalid name.
	_, err := svc.Create(createReq("bad;rm -rf /"))

	// Assert: a validation fault and no Tart command.
	if fe := faultOf(t, err); fe.Fault != FaultValidation {
		t.Fatalf("fault = %q, want %q", fe.Fault, FaultValidation)
	}
	if len(tart.clones) != 0 {
		t.Fatalf("clone ran %d times, want 0", len(tart.clones))
	}
}

func TestCreateIdempotentOnName(t *testing.T) {
	// Arrange: a ready VM.
	tart := newFakeTart()
	svc := newTestService(t, tart)
	if _, err := svc.Create(createReq("garm-abc123")); err != nil {
		t.Fatal(err)
	}

	// Act: create the same name again.
	inst, err := svc.Create(createReq("garm-abc123"))

	// Assert: the existing record is returned, no second clone.
	if err != nil {
		t.Fatalf("second Create error = %v, want nil", err)
	}
	if inst.State != StateReady {
		t.Fatalf("state = %s, want ready", inst.State)
	}
	if len(tart.clones) != 1 {
		t.Fatalf("clones = %d, want 1", len(tart.clones))
	}
}

func TestStaleRunningRecordReprobedToReady(t *testing.T) {
	// Arrange: a running record whose readiness probe failed earlier and whose
	// guest now answers, sitting past the stale-create TTL.
	tart := newFakeTart()
	svc := newTestService(t, tart)
	svc.staleCreateTTL = time.Minute
	now := time.Now()
	svc.now = func() time.Time { return now }
	tart.mu.Lock()
	tart.vms["garm-stuck1"] = VM{Name: "garm-stuck1", State: "running"}
	tart.mu.Unlock()
	rec := Record{
		Name:         "garm-stuck1",
		ProviderID:   "mac-mini-01/garm-stuck1",
		ControllerID: "controller-1",
		State:        StateRunning,
		LastError:    "guest agent not up",
		CreatedAt:    now.Add(-2 * time.Hour),
		UpdatedAt:    now.Add(-2 * time.Minute),
	}
	if err := svc.reg.Save(rec); err != nil {
		t.Fatal(err)
	}

	// Act: reconcile.
	if err := svc.Reconcile(); err != nil {
		t.Fatal(err)
	}

	// Assert: the record advanced to ready with last_error cleared.
	got, err := svc.reg.Load("garm-stuck1")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != StateReady {
		t.Fatalf("state = %s, want ready", got.State)
	}
	if got.LastError != "" {
		t.Fatalf("last_error = %q, want cleared", got.LastError)
	}
}

func TestStaleRunningRecordReapedWhenProbeStillFails(t *testing.T) {
	// Arrange: a running record past the TTL whose guest still does not answer.
	tart := newFakeTart()
	tart.execErr = fmt.Errorf("guest agent not responding")
	svc := newTestService(t, tart)
	svc.staleCreateTTL = time.Minute
	now := time.Now()
	svc.now = func() time.Time { return now }
	tart.mu.Lock()
	tart.vms["garm-stuck2"] = VM{Name: "garm-stuck2", State: "running"}
	tart.mu.Unlock()
	rec := Record{
		Name:         "garm-stuck2",
		ProviderID:   "mac-mini-01/garm-stuck2",
		ControllerID: "controller-1",
		State:        StateRunning,
		CreatedAt:    now.Add(-2 * time.Hour),
		UpdatedAt:    now.Add(-2 * time.Minute),
	}
	if err := svc.reg.Save(rec); err != nil {
		t.Fatal(err)
	}

	// Act: reconcile.
	if err := svc.Reconcile(); err != nil {
		t.Fatal(err)
	}

	// Assert: the record is reaped and the VM deleted.
	if _, err := svc.reg.Load("garm-stuck2"); err != ErrNotFound {
		t.Fatalf("record still present (err=%v), want reaped", err)
	}
	tart.mu.Lock()
	_, present := tart.vms["garm-stuck2"]
	tart.mu.Unlock()
	if present {
		t.Fatal("VM still present in tart list after reap")
	}
}

func TestStaleCloningRecordReaped(t *testing.T) {
	// Arrange: a cloning record past the TTL — a create killed mid-clone/set.
	tart := newFakeTart()
	svc := newTestService(t, tart)
	svc.staleCreateTTL = time.Minute
	now := time.Now()
	svc.now = func() time.Time { return now }
	tart.mu.Lock()
	tart.vms["garm-stuck3"] = VM{Name: "garm-stuck3", State: "stopped"}
	tart.mu.Unlock()
	rec := Record{
		Name:         "garm-stuck3",
		ProviderID:   "mac-mini-01/garm-stuck3",
		ControllerID: "controller-1",
		State:        StateCloning,
		CreatedAt:    now.Add(-2 * time.Hour),
		UpdatedAt:    now.Add(-2 * time.Minute),
	}
	if err := svc.reg.Save(rec); err != nil {
		t.Fatal(err)
	}

	// Act: reconcile.
	if err := svc.Reconcile(); err != nil {
		t.Fatal(err)
	}

	// Assert: cloning has no resume path, so the record is reaped outright.
	if _, err := svc.reg.Load("garm-stuck3"); err != ErrNotFound {
		t.Fatalf("record still present (err=%v), want reaped", err)
	}
	tart.mu.Lock()
	_, present := tart.vms["garm-stuck3"]
	tart.mu.Unlock()
	if present {
		t.Fatal("VM still present in tart list after reap")
	}
}

func TestStaleReaperPreservesRecordWhenTartDeleteLies(t *testing.T) {
	tart := newFakeTart()
	tart.deleteNoop = true
	svc := newTestService(t, tart)
	now := time.Now()
	svc.now = func() time.Time { return now }
	tart.vms["garm-stuck4"] = VM{Name: "garm-stuck4", State: "stopped"}
	if err := svc.reg.Save(Record{
		Name:         "garm-stuck4",
		ControllerID: "controller-1",
		State:        StateCloning,
		CreatedAt:    now.Add(-time.Hour),
		UpdatedAt:    now.Add(-time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	if err := svc.Reconcile(); err != nil {
		t.Fatal(err)
	}
	if rec, err := svc.reg.Load("garm-stuck4"); err != nil || rec.State != StateCloning {
		t.Fatalf("stale VM record released without proof: rec=%+v err=%v", rec, err)
	}
}

func TestFreshRunningRecordLeftAlone(t *testing.T) {
	// Arrange: a running record younger than the stale-create TTL.
	tart := newFakeTart()
	tart.execErr = fmt.Errorf("guest agent not responding")
	svc := newTestService(t, tart)
	svc.staleCreateTTL = time.Minute
	now := time.Now()
	svc.now = func() time.Time { return now }
	tart.mu.Lock()
	tart.vms["garm-fresh1"] = VM{Name: "garm-fresh1", State: "running"}
	tart.mu.Unlock()
	rec := Record{
		Name:         "garm-fresh1",
		ProviderID:   "mac-mini-01/garm-fresh1",
		ControllerID: "controller-1",
		State:        StateRunning,
		LastError:    "guest agent not up",
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	if err := svc.reg.Save(rec); err != nil {
		t.Fatal(err)
	}

	// Act: reconcile.
	if err := svc.Reconcile(); err != nil {
		t.Fatal(err)
	}

	// Assert: the record is untouched and the VM survives.
	got, err := svc.reg.Load("garm-fresh1")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != StateRunning {
		t.Fatalf("state = %s, want running (left alone)", got.State)
	}
	tart.mu.Lock()
	_, present := tart.vms["garm-fresh1"]
	tart.mu.Unlock()
	if !present {
		t.Fatal("VM was deleted despite the record being fresh")
	}
}
