package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	commonParams "github.com/cloudbase/garm-provider-common/params"

	"github.com/yanksyoon/garm-provider-tart/internal/hostctl"
)

// runner runs one hostctl verb on one host and returns its stdout (JSON). The
// concrete implementation is *Client (the SSH transport); tests substitute an
// in-memory fake to simulate a fleet without exec'ing ssh.
type runner interface {
	Run(ctx context.Context, verb string, args ...string) ([]byte, error)
	RunInput(ctx context.Context, stdin io.Reader, verb string, args ...string) ([]byte, error)
}

// Provider implements the GARM external-provider operations by driving the
// host-owned tart-hostctl registry over SSH. The host is the source of truth;
// this provider holds no state of its own.
type Provider struct {
	cfg          *Config
	controllerID string
	stderr       io.Writer

	// clientFor builds the runner for one host. Tests replace it with a
	// fake-fleet factory; it defaults to the real SSH client.
	clientFor func(Host) runner

	// waitMargin is the portion of the create timeout reserved for the create
	// itself after a capacity wait (create_timeout − waitMargin).
	waitMargin time.Duration
	// probeInterval returns the delay between capacity-wait re-probes.
	probeInterval func() time.Duration
}

// New builds a provider from a loaded config.
func New(cfg *Config, controllerID string) *Provider {
	return &Provider{
		cfg:           cfg,
		controllerID:  controllerID,
		stderr:        os.Stderr,
		waitMargin:    defaultWaitMargin,
		probeInterval: jitterInterval,
	}
}

// client builds the runner for one host.
func (p *Provider) client(host Host) runner {
	if p.clientFor != nil {
		return p.clientFor(host)
	}
	return NewClient(host, p.cfg.SSHUser, p.cfg.SSHKey, p.cfg.KnownHosts, p.cfg.SSHBin, p.cfg.HostctlBin)
}

// CreateInstance renders the bootstrap JSON on r, places the VM on the fleet
// via the placement loop, clones and boots it, and prints the mapped
// ProviderInstance. A placement fault (no_reachable_host / capacity) is
// reported as an errored ProviderInstance so GARM distinguishes it in
// provider_fault; a transport or tart failure is a hard error GARM retries.
func (p *Provider) CreateInstance(r io.Reader, w io.Writer) error {
	var bootstrap commonParams.BootstrapInstance
	if err := json.NewDecoder(r).Decode(&bootstrap); err != nil {
		return fmt.Errorf("decode bootstrap params from stdin: %w", err)
	}
	if bootstrap.Name == "" {
		return fmt.Errorf("bootstrap params carry no instance name")
	}
	specs := parseExtraSpecs(bootstrap.ExtraSpecs)
	image := bootstrap.Image
	if image == "" {
		image = p.cfg.Image
	}

	ctx, cancel := context.WithTimeout(context.Background(), p.cfg.CreateTimeout())
	defer cancel()

	started := time.Now()
	inst, host, err := p.placeAndCreate(ctx, bootstrap.Name, image, specs, bootstrap.PoolID)
	if err != nil {
		var fe *FaultError
		if errors.As(err, &fe) && (fe.Fault == FaultNoReachableHost || fe.Fault == hostctl.FaultCapacity) {
			p.metric("create_latency_ms=%d outcome=%s host=%s", time.Since(started).Milliseconds(), fe.Fault, host.Name)
			return writeJSON(w, faultInstance(host, bootstrap.Name, fe.Fault, fe.Message))
		}
		p.metric("create_latency_ms=%d outcome=error host=%s", time.Since(started).Milliseconds(), host.Name)
		return err
	}
	if strings.TrimSpace(specs.RunnerInstallTemplate) != "" {
		// GARM supplies the rendered runner-install wrapper in extra_specs.
		// Feed it over SSH stdin so it cannot appear in argv or the host
		// registry. The guest runs the wrapper via `tart exec -i`.
		out, err := p.client(host).RunInput(ctx, strings.NewReader(specs.RunnerInstallTemplate), "bootstrap", "--name", bootstrap.Name)
		if err != nil {
			p.cleanup(host, bootstrap.Name)
			return fmt.Errorf("bootstrap %q on %s: %w", bootstrap.Name, host.Name, err)
		}
		if err := json.Unmarshal(out, &inst); err != nil {
			p.cleanup(host, bootstrap.Name)
			return fmt.Errorf("parse hostctl bootstrap output from %s: %w", host.Name, err)
		}
		if inst.State != hostctl.StateIdle || inst.Name != bootstrap.Name || inst.ProviderID != host.Name+"/"+bootstrap.Name {
			p.cleanup(host, bootstrap.Name)
			return fmt.Errorf("hostctl bootstrap on %s returned unexpected instance state or identity: %s/%s (%s)", host.Name, inst.ProviderID, inst.Name, inst.State)
		}
	} else {
		p.logf("create %q on %s: no runner_install_template provided; guest has no registered runner", bootstrap.Name, host.Name)
	}
	p.metric("create_latency_ms=%d outcome=ok host=%s", time.Since(started).Milliseconds(), host.Name)
	return writeJSON(w, p.providerInstance(inst))
}

// GetInstance reports the state of one instance by asking its host.
func (p *Provider) GetInstance(instanceID string, w io.Writer) error {
	if instanceID == "" {
		return fmt.Errorf("GARM_INSTANCE_ID is not set")
	}
	host, name, err := p.splitInstanceID(instanceID)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), p.cfg.CommandTimeout())
	defer cancel()
	out, err := p.client(host).Run(ctx, "get", "--name", name)
	if err != nil {
		var fe *FaultError
		if errors.As(err, &fe) && fe.Fault == hostctl.FaultNotFound {
			return fmt.Errorf("instance %q not found on %s", name, host.Name)
		}
		return err
	}
	var inst hostctl.Instance
	if err := json.Unmarshal(out, &inst); err != nil {
		return fmt.Errorf("parse hostctl get output: %w", err)
	}
	return writeJSON(w, p.providerInstance(inst))
}

// DeleteInstance removes one instance via hostctl's delete verb. It is a no-op
// when the VM is gone, and a hard error when the host is unreachable (so GARM
// retries and the host-side tombstone keeps the slot counted).
func (p *Provider) DeleteInstance(instanceID string) error {
	if instanceID == "" {
		return fmt.Errorf("GARM_INSTANCE_ID is not set")
	}
	host, name, err := p.splitInstanceID(instanceID)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), p.cfg.CommandTimeout())
	defer cancel()
	_, err = p.client(host).Run(ctx, "delete", "--name", name)
	return err
}

// Start boots a stopped instance. Not implemented until stop/start semantics
// land with the bootstrap template (T3).
func (p *Provider) Start(instanceID string) error {
	if instanceID == "" {
		return fmt.Errorf("GARM_INSTANCE_ID is not set")
	}
	return fmt.Errorf("start is not implemented")
}

// Stop shuts a running instance down without deleting it.
func (p *Provider) Stop(instanceID string) error {
	if instanceID == "" {
		return fmt.Errorf("GARM_INSTANCE_ID is not set")
	}
	return fmt.Errorf("stop is not implemented")
}

// providerInstance maps a hostctl instance to the GARM view. Only `ready` (and
// its post-bootstrap states) maps to GARM `running`; a `running` record whose
// readiness probe failed maps to `error` with the last_error as provider_fault.
func (p *Provider) providerInstance(inst hostctl.Instance) commonParams.ProviderInstance {
	pi := commonParams.ProviderInstance{
		ProviderID: inst.ProviderID,
		Name:       inst.Name,
		OSType:     commonParams.Linux,
		OSName:     "macos",
		OSVersion:  "26",
		OSArch:     commonParams.Arm64,
		Status:     mapStatus(inst.State),
	}
	if inst.IP != "" {
		pi.Addresses = []commonParams.Address{{Address: inst.IP, Type: commonParams.PrivateAddress}}
	}
	if inst.State == hostctl.StateRunning && inst.LastError != "" {
		pi.ProviderFault = []byte(inst.LastError)
	}
	return pi
}

// faultInstance is the placement-fault answer: an errored ProviderInstance whose
// provider_fault names the fault so GARM can tell no_reachable_host from
// capacity.
func faultInstance(host Host, name, fault, msg string) commonParams.ProviderInstance {
	return commonParams.ProviderInstance{
		ProviderID:    host.Name + "/" + name,
		Name:          name,
		OSType:        commonParams.Linux,
		OSName:        "macos",
		OSArch:        commonParams.Arm64,
		Status:        commonParams.InstanceError,
		ProviderFault: []byte(fault + ": " + msg),
	}
}

// mapStatus translates a hostctl state into a GARM instance status.
func mapStatus(s hostctl.State) commonParams.InstanceStatus {
	switch s {
	case hostctl.StateReady, hostctl.StateBootstrapping, hostctl.StateIdle, hostctl.StateAdopted:
		return commonParams.InstanceRunning
	case hostctl.StateCloning:
		return commonParams.InstanceCreating
	case hostctl.StateRunning, hostctl.StateFailed:
		return commonParams.InstanceError
	case hostctl.StateDeleting:
		return commonParams.InstanceDeleting
	default:
		return commonParams.InstanceStatusUnknown
	}
}

// extraSpecs is the subset of the pool extra_specs the provider understands:
// per-VM resource overrides, a host pin and GARM's rendered runner-install
// wrapper. The wrapper is sent only over SSH stdin.
type extraSpecs struct {
	Host                  string `json:"host"`
	CPU                   int    `json:"cpu"`
	MemoryMB              int    `json:"mem"`
	DiskGB                int    `json:"disk"`
	RunnerInstallTemplate string `json:"runner_install_template"`
}

func parseExtraSpecs(raw json.RawMessage) extraSpecs {
	var specs extraSpecs
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &specs)
	}
	return specs
}

func (s extraSpecs) cpu(cfg *Config) int {
	if s.CPU > 0 {
		return s.CPU
	}
	return cfg.DefaultCPU
}

func (s extraSpecs) mem(cfg *Config) int {
	if s.MemoryMB > 0 {
		return s.MemoryMB
	}
	return cfg.DefaultMemoryMB
}

func (s extraSpecs) disk(cfg *Config) int {
	if s.DiskGB > 0 {
		return s.DiskGB
	}
	return cfg.DefaultDiskGB
}

// splitInstanceID parses "<host>/<vm-name>", or accepts a bare GARM instance
// name, which GARM sends when a create failed before a provider ID existed.
func (p *Provider) splitInstanceID(instanceID string) (Host, string, error) {
	for _, host := range p.cfg.Hosts {
		prefix := host.Name + "/"
		if len(instanceID) > len(prefix) && instanceID[:len(prefix)] == prefix {
			return host, instanceID[len(prefix):], nil
		}
	}
	return p.cfg.Hosts[0], instanceID, nil
}

func writeJSON(w io.Writer, value any) error {
	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)
	return encoder.Encode(value)
}
