package hostctl

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

// Service is the host-owned control plane: the registry, the Tart verbs, and
// the host flock that serialises every mutation. One Service per hostctl
// process; the process exits after a single verb, so the flock is held for the
// whole verb and concurrent verbs queue rather than interleave.
type Service struct {
	root           string
	reg            *Registry
	tart           Tart
	health         Health
	hostName       string
	controllerID   string
	slotsPerHost   int
	quarantineTTL  time.Duration
	staleCreateTTL time.Duration
	probeTimeout   time.Duration
	now            func() time.Time
}

// Options configures a Service. Root is the fleet directory
// (/var/db/tart-fleet). SlotsPerHost, QuarantineTTL, StaleCreateTTL and
// ProbeTimeout get defaults when zero.
type Options struct {
	Root           string
	Tart           Tart
	Health         Health
	HostName       string
	ControllerID   string
	SlotsPerHost   int
	QuarantineTTL  time.Duration
	StaleCreateTTL time.Duration
	ProbeTimeout   time.Duration
}

// defaultStaleCreateTTL is how long a record may sit in `cloning` or `running`
// before Reconcile treats the create as interrupted (a crash) and reclaims it —
// re-probing a running record toward ready, or reaping the VM and record. It is
// far longer than a normal create (the exclusive flock already prevents
// Reconcile from observing an in-flight create), so it only bounds how long a
// crash can strand a slot.
const defaultStaleCreateTTL = 5 * time.Minute

// defaultProbeTimeout bounds the `tart exec <name> true` readiness probe so a
// guest whose agent never answers cannot pin the exclusive host flock. It is
// generous enough for a first boot's guest-agent startup and short enough that
// a hung guest releases the flock in bounded time; operators tune it down via
// TART_FLEET_PROBE_TIMEOUT_SECONDS when a half-booted VM must fail faster.
const defaultProbeTimeout = 60 * time.Second

// MaxSlotsPerHost caps concurrent guests per Mac. The macOS licence agreement
// (section 2.B.iii) allows at most two virtualised instances per Apple-branded
// host, so a larger value is a licence breach rather than a tuning choice. It
// is the hard ceiling enforced by NewService, mirroring the provider's
// maxSlotsPerHost.
const MaxSlotsPerHost = 2

// NewService builds a Service. A nil Health yields an always-healthy probe so
// the CLI works on hosts where the keychain check is unavailable; a nil Tart
// yields a real `tart` runner. A SlotsPerHost above MaxSlotsPerHost is
// rejected: the macOS licence caps concurrent guests per host at two.
func NewService(o Options) (*Service, error) {
	if o.SlotsPerHost == 0 {
		o.SlotsPerHost = MaxSlotsPerHost
	}
	if o.SlotsPerHost > MaxSlotsPerHost {
		return nil, &FaultError{
			Fault:   FaultValidation,
			Message: fmt.Sprintf("slots per host %d exceeds the macOS licence cap of %d guests per host", o.SlotsPerHost, MaxSlotsPerHost),
		}
	}
	if o.QuarantineTTL == 0 {
		o.QuarantineTTL = 24 * time.Hour
	}
	if o.StaleCreateTTL == 0 {
		o.StaleCreateTTL = defaultStaleCreateTTL
	}
	if o.ProbeTimeout == 0 {
		o.ProbeTimeout = defaultProbeTimeout
	}
	if o.HostName == "" {
		o.HostName = hostname()
	}
	if o.Tart == nil {
		o.Tart = NewRealTart("")
	}
	if o.Health == nil {
		o.Health = noopHealth{}
	}
	return &Service{
		root:           o.Root,
		reg:            NewRegistry(o.Root),
		tart:           o.Tart,
		health:         o.Health,
		hostName:       o.HostName,
		controllerID:   o.ControllerID,
		slotsPerHost:   o.SlotsPerHost,
		quarantineTTL:  o.QuarantineTTL,
		staleCreateTTL: o.StaleCreateTTL,
		probeTimeout:   o.ProbeTimeout,
		now:            time.Now,
	}, nil
}

// noopHealth reports an unlocked keychain and zero free disk when no real probe
// is configured.
type noopHealth struct{}

func (noopHealth) KeychainUnlocked() (bool, error) { return true, nil }
func (noopHealth) FreeDiskBytes() (uint64, error)  { return 0, nil }

// lock acquires the host flock for the duration of fn. Read-only verbs use a
// shared lock; every mutation uses an exclusive lock held for the whole verb.
func (s *Service) lock(exclusive bool, fn func() error) error {
	if err := os.MkdirAll(s.root, 0o700); err != nil {
		return fmt.Errorf("create fleet dir: %w", err)
	}
	lf, err := os.OpenFile(filepath.Join(s.root, "lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("open fleet lock: %w", err)
	}
	how := unix.LOCK_SH
	if exclusive {
		how = unix.LOCK_EX
	}
	if err := unix.Flock(int(lf.Fd()), how); err != nil {
		lf.Close()
		return fmt.Errorf("lock fleet: %w", err)
	}
	defer func() {
		_ = unix.Flock(int(lf.Fd()), unix.LOCK_UN)
		_ = lf.Close()
	}()
	return fn()
}

func (s *Service) drained() (bool, error) {
	_, err := os.Stat(filepath.Join(s.root, "drained"))
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

func (s *Service) setDrained(d bool) error {
	path := filepath.Join(s.root, "drained")
	if d {
		return os.WriteFile(path, []byte("drained\n"), 0o600)
	}
	err := os.Remove(path)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// Create implements the `create` verb. Under the exclusive lock it validates the
// name and image, refuses on a locked keychain, drain or full host, then clones,
// persists its controller in the host registry, sets resources, launches Tart
// headless in a detached session and probes readiness with `tart exec true`. It returns the resulting
// record; a failed readiness probe leaves the record in state `running` with
// last_error so the caller can decide.
func (s *Service) Create(req CreateRequest) (Instance, error) {
	var result Instance
	err := s.lock(true, func() error {
		inst, err := s.create(req)
		if err != nil {
			return err
		}
		result = inst
		return nil
	})
	return result, err
}

// CreateRequest is the argument set for the create verb.
type CreateRequest struct {
	Name       string
	Image      string
	CPU        int
	MemoryMB   int
	DiskGB     int
	Controller string
	Owner      string
	Host       string
}

func (s *Service) create(req CreateRequest) (Instance, error) {
	if !ValidName(req.Name) {
		return Instance{}, &FaultError{Fault: FaultValidation, Message: fmt.Sprintf("invalid name %q: must match ^[a-z0-9]([-a-z0-9]{0,50})$", req.Name)}
	}
	if req.Image == "" {
		return Instance{}, &FaultError{Fault: FaultValidation, Message: "image is required"}
	}
	if req.Controller == "" {
		return Instance{}, &FaultError{Fault: FaultValidation, Message: "controller is required"}
	}

	// Health gate before any Tart command runs.
	unlocked, err := s.health.KeychainUnlocked()
	if err != nil {
		return Instance{}, fmt.Errorf("check keychain: %w", err)
	}
	if !unlocked {
		return Instance{}, &FaultError{Fault: FaultKeychain, Message: "login keychain is locked; run `security unlock-keychain` (or enable auto-login) before creating VMs"}
	}

	drained, err := s.drained()
	if err != nil {
		return Instance{}, err
	}
	if drained {
		return Instance{}, &FaultError{Fault: FaultDrained, Message: "host is drained; new creates are refused"}
	}

	// Reconcile so capacity is honest after a crash, then enforce the cap.
	if err := s.Reconcile(); err != nil {
		return Instance{}, fmt.Errorf("reconcile: %w", err)
	}

	// Idempotency: a create for an already-tracked, live name returns the
	// existing record instead of cloning a second VM with the same name.
	if existing, err := s.reg.Load(req.Name); err == nil {
		switch existing.State {
		case StateDeleting, StateGone:
			// A tombstone or a gone record does not represent a live VM;
			// fall through and create fresh.
		default:
			return s.instance(existing, s.inTart(req.Name)), nil
		}
	}

	if err := s.enforceCapacity(req.Name); err != nil {
		return Instance{}, err
	}

	host := req.Host
	if host == "" {
		host = s.hostName
	}
	providerID := host + "/" + req.Name
	now := s.now()

	rec := Record{
		Name:         req.Name,
		ProviderID:   providerID,
		ControllerID: req.Controller,
		OwnerID:      req.Owner,
		Image:        req.Image,
		State:        StateCloning,
		Lease:        Lease{Holder: req.Controller, AcquiredAt: now, TTLSec: defaultLeaseTTLSeconds},
		CreatedAt:    now,
		UpdatedAt:    now,
	}

	// clone → record write. A crash in this window leaves a VM with no record,
	// which the next Reconcile adopts (label set below) or quarantines.
	if err := s.tart.Clone(req.Image, req.Name); err != nil {
		return Instance{}, &FaultError{Fault: FaultTart, Message: err.Error()}
	}
	if err := s.reg.Save(rec); err != nil {
		return Instance{}, fmt.Errorf("record cloning state: %w", err)
	}

	if err := s.tart.SetLabels(req.Name, map[string]string{controllerLabelKey: req.Controller}); err != nil {
		return Instance{}, &FaultError{Fault: FaultTart, Message: err.Error()}
	}
	if err := s.tart.SetResources(req.Name, req.CPU, req.MemoryMB, req.DiskGB); err != nil {
		return Instance{}, &FaultError{Fault: FaultTart, Message: err.Error()}
	}

	if err := s.tart.Run(req.Name); err != nil {
		rec.State = StateFailed
		rec.LastError = err.Error()
		rec.UpdatedAt = s.now()
		rec.BootAttempts++
		_ = s.reg.Save(rec)
		return Instance{}, &FaultError{Fault: FaultTart, Message: err.Error()}
	}

	rec.State = StateRunning
	rec.BootAttempts++
	rec.UpdatedAt = s.now()
	if err := s.reg.Save(rec); err != nil {
		return Instance{}, err
	}

	// Readiness means `tart exec <name> true` succeeded — not that `tart run`
	// returned. The probe is bounded by the host so a guest whose agent never
	// answers cannot pin the exclusive flock: on timeout the record stays
	// running with last_error and the flock is released for other verbs.
	probeCtx, cancel := context.WithTimeout(context.Background(), s.probeTimeout)
	err = s.tart.Exec(probeCtx, req.Name, nil, "true")
	cancel()
	if err != nil {
		rec.LastError = err.Error()
		rec.UpdatedAt = s.now()
		_ = s.reg.Save(rec)
		return s.instance(rec, true), nil
	}
	rec.State = StateReady
	rec.LastError = ""
	rec.UpdatedAt = s.now()
	if err := s.reg.Save(rec); err != nil {
		return Instance{}, err
	}

	return s.instance(rec, true), nil
}

// enforceCapacity refuses a create when the host is at its licence cap, counting
// every live record (including deleting tombstones) plus adopted VMs. The
// requested name is not yet a record, so a retry of the same name that failed
// mid-create is still counted against capacity via its tombstone.
func (s *Service) enforceCapacity(name string) error {
	used, adopted, _, err := s.capacity()
	if err != nil {
		return err
	}
	if used+adopted >= s.slotsPerHost {
		return &FaultError{Fault: FaultCapacity, Message: fmt.Sprintf("no free slot: %d of %d slots in use", used+adopted, s.slotsPerHost)}
	}
	return nil
}

// capacity counts records by bucket. used is every live lifecycle record;
// adopted and quarantined are the reconciliation states.
func (s *Service) capacity() (used, adopted, quarantined int, err error) {
	records, err := s.reg.All()
	if err != nil {
		return 0, 0, 0, err
	}
	for _, rec := range records {
		switch rec.State {
		case StateAdopted:
			adopted++
		case StateQuarantined:
			quarantined++
		default:
			if rec.State.IsLive() {
				used++
			}
		}
	}
	return used, adopted, quarantined, nil
}

// instance renders the live view of a record: its registry state plus whether
// Tart still lists it and its current IP.
func (s *Service) instance(rec Record, inTart bool) Instance {
	ip, _ := s.tart.IP(rec.Name)
	return Instance{Record: rec, IP: ip, InTart: inTart}
}

// Get implements the `get` verb.
func (s *Service) Get(name string) (Instance, error) {
	if !ValidName(name) {
		return Instance{}, &FaultError{Fault: FaultValidation, Message: fmt.Sprintf("invalid name %q", name)}
	}
	var result Instance
	err := s.lock(false, func() error {
		rec, err := s.reg.Load(name)
		if err != nil {
			if err == ErrNotFound {
				return &FaultError{Fault: FaultNotFound, Message: fmt.Sprintf("no record for %q", name)}
			}
			return err
		}
		inTart := s.inTart(name)
		result = s.instance(rec, inTart)
		return nil
	})
	return result, err
}

// inTart reports whether name is present in `tart list` right now.
func (s *Service) inTart(name string) bool {
	vms, err := s.tart.List()
	if err != nil {
		return false
	}
	for _, vm := range vms {
		if vm.Name == name {
			return true
		}
	}
	return false
}

// List implements the `list` verb, optionally filtered by controller or owner.
// Quarantined VMs are excluded: they are not GARM instances.
func (s *Service) List(controller, owner string) ([]Instance, error) {
	var result []Instance
	err := s.lock(false, func() error {
		instances, err := s.listLocked(controller, owner)
		if err != nil {
			return err
		}
		result = instances
		return nil
	})
	return result, err
}

// listLocked is the lock-free body of List; Status calls it while it already
// holds the exclusive lock, since a verb must not re-acquire the flock it owns.
func (s *Service) listLocked(controller, owner string) ([]Instance, error) {
	records, err := s.reg.All()
	if err != nil {
		return nil, err
	}
	vms, err := s.tart.List()
	if err != nil {
		return nil, err
	}
	byName := make(map[string]VM, len(vms))
	for _, vm := range vms {
		byName[vm.Name] = vm
	}
	result := make([]Instance, 0, len(records))
	for _, rec := range records {
		if rec.State == StateQuarantined {
			continue
		}
		if controller != "" && rec.ControllerID != controller {
			continue
		}
		if owner != "" && rec.OwnerID != owner {
			continue
		}
		_, inTart := byName[rec.Name]
		result = append(result, s.instance(rec, inTart))
	}
	return result, nil
}

// Delete implements the `delete` verb, proving release: stop (bounded) → tart
// delete → confirm absent from `tart list` → remove the record → release the
// slot. An absent VM is a no-op success. A Tart failure keeps the deleting
// tombstone (slot still counted) and returns an error.
func (s *Service) Delete(name string) error {
	if !ValidName(name) {
		return &FaultError{Fault: FaultValidation, Message: fmt.Sprintf("invalid name %q", name)}
	}
	return s.lock(true, func() error {
		return s.delete(name)
	})
}

func (s *Service) delete(name string) error {
	rec, err := s.reg.Load(name)
	exists := err == nil
	if err != nil && err != ErrNotFound {
		return err
	}

	if !exists && !s.inTart(name) {
		// Absent from both registry and tart list: a no-op success.
		return nil
	}

	// Mark the tombstone so the slot stays counted for the whole deletion.
	now := s.now()
	if exists {
		rec.State = StateDeleting
		rec.UpdatedAt = now
		if err := s.reg.Save(rec); err != nil {
			return err
		}
	}

	_ = s.tart.Stop(name) // bounded by the transport timeout; best-effort
	if err := s.tart.Delete(name); err != nil {
		return &FaultError{Fault: FaultTart, Message: fmt.Sprintf("delete %q: %v", name, err)}
	}
	if s.inTart(name) {
		return &FaultError{Fault: FaultTart, Message: fmt.Sprintf("delete %q did not release: still present in tart list", name)}
	}
	if err := s.reg.Delete(name); err != nil {
		return err
	}
	return nil
}

// Bootstrap implements the `bootstrap` verb: it requires a ready instance,
// marks it bootstrapping, runs the supplied script inside the guest via
// `tart exec -i <name> /bin/bash -s`, and records idle or failed. The token is
// never persisted.
func (s *Service) Bootstrap(name string, script io.Reader) (Instance, error) {
	if !ValidName(name) {
		return Instance{}, &FaultError{Fault: FaultValidation, Message: fmt.Sprintf("invalid name %q", name)}
	}
	var result Instance
	err := s.lock(true, func() error {
		rec, err := s.reg.Load(name)
		if err != nil {
			if err == ErrNotFound {
				return &FaultError{Fault: FaultNotFound, Message: fmt.Sprintf("no record for %q", name)}
			}
			return err
		}
		if rec.State != StateReady && rec.State != StateIdle {
			return &FaultError{Fault: FaultTart, Message: fmt.Sprintf("instance %q is %s, not ready", name, rec.State)}
		}
		rec.State = StateBootstrapping
		rec.UpdatedAt = s.now()
		if err := s.reg.Save(rec); err != nil {
			return err
		}
		// The bootstrap script may legitimately run long (downloading the
		// runner, configuring launchd), so it is not host-bounded: the
		// provider's ssh deadline on the create verb is the bound, not the
		// readiness-probe timeout.
		if err := s.tart.Exec(context.Background(), name, script, "/bin/bash", "-s"); err != nil {
			rec.State = StateFailed
			rec.LastError = err.Error()
			rec.UpdatedAt = s.now()
			_ = s.reg.Save(rec)
			return &FaultError{Fault: FaultTart, Message: err.Error()}
		}
		rec.State = StateIdle
		rec.LastError = ""
		rec.UpdatedAt = s.now()
		if err := s.reg.Save(rec); err != nil {
			return err
		}
		result = s.instance(rec, true)
		return nil
	})
	return result, err
}

// Drain refuses new creates without touching running VMs.
func (s *Service) Drain() error {
	return s.lock(true, func() error { return s.setDrained(true) })
}

// Undrain reverses a drain.
func (s *Service) Undrain() error {
	return s.lock(true, func() error { return s.setDrained(false) })
}

// Status implements the `status --json` verb: it reconciles, then reports the
// health gate.
func (s *Service) Status() (Status, error) {
	var result Status
	err := s.lock(true, func() error {
		if err := s.Reconcile(); err != nil {
			return err
		}
		version, err := s.tart.Version()
		if err != nil {
			return err
		}
		unlocked, err := s.health.KeychainUnlocked()
		if err != nil {
			return err
		}
		free, err := s.health.FreeDiskBytes()
		if err != nil {
			return err
		}
		drained, err := s.drained()
		if err != nil {
			return err
		}
		used, adopted, quarantined, err := s.capacity()
		if err != nil {
			return err
		}
		instances, err := s.listLocked("", "")
		if err != nil {
			return err
		}
		result = Status{
			TartVersion:      version,
			KeychainUnlocked: unlocked,
			FreeDiskBytes:    free,
			Drained:          drained,
			Capacity: Capacity{
				Total:       s.slotsPerHost,
				Used:        used,
				Adopted:     adopted,
				Quarantined: quarantined,
			},
			Instances: instances,
		}
		return nil
	})
	return result, err
}

// FaultError is a classified verb failure. Its Fault field maps to the wire
// Fault name and, for the provider, to whether GARM should treat it as a
// capacity fault or a hard error.
type FaultError struct {
	Fault   string
	Message string
}

func (e *FaultError) Error() string { return e.Message }

// MarshalJSON renders the fault the way the CLI writes it to stderr.
func (e *FaultError) MarshalJSON() ([]byte, error) {
	return json.Marshal(Fault{Fault: e.Fault, Error: e.Message})
}
