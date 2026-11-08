package provider

import (
	"fmt"
	"os"
	"time"

	"github.com/BurntSushi/toml"
)

// maxSlotsPerHost caps concurrent guests per Mac. The macOS licence agreement
// (section 2.B.iii) allows at most two virtualised instances per Apple-branded
// host, so a larger value is a licence breach rather than a tuning choice.
const maxSlotsPerHost = 2

const (
	defaultSSHPort              = 22
	defaultCreateTimeoutSeconds = 900
	defaultCommandTimeoutSecs   = 30
	defaultProbeTimeoutSeconds  = 3
	defaultHostctlBin           = "/usr/local/bin/tart-hostctl"
	defaultCPU                  = 4
	defaultMemoryMB             = 8192
	defaultDiskGB               = 50
)

// Host is one Apple Silicon Mac mini that runs Tart VMs.
type Host struct {
	// Name is the stable identifier recorded in provider IDs.
	Name string `toml:"name"`
	// Address is the SSH target, host name or IP.
	Address string `toml:"address"`
	// Port is the SSH port. Zero means the default (22).
	Port int `toml:"port"`
}

// port returns the SSH port, defaulting to 22.
func (h Host) port() int {
	if h.Port == 0 {
		return defaultSSHPort
	}
	return h.Port
}

// Config mirrors the provider's TOML config file, whose path GARM passes in
// GARM_PROVIDER_CONFIG_FILE.
type Config struct {
	// SSHUser is the unprivileged account the provider uses on each host.
	SSHUser string `toml:"ssh_user"`
	// SSHKey is the path to the private key for SSHUser.
	SSHKey string `toml:"ssh_key"`
	// KnownHosts is the path to an OpenSSH known_hosts file. When empty, new
	// keys are trusted on first use; changed keys are still rejected.
	KnownHosts string `toml:"known_hosts"`
	// SSHBin is the ssh client binary (default "ssh"). Overridable so the
	// break-glass command wrapper, or tests, can substitute another binary.
	SSHBin string `toml:"ssh_bin"`
	// HostctlBin is the tart-hostctl binary on each host.
	HostctlBin string `toml:"hostctl_bin"`

	// CreateTimeoutSeconds bounds one CreateInstance call end to end.
	CreateTimeoutSeconds int `toml:"create_timeout_seconds"`
	// CommandTimeoutSeconds bounds one SSH command on a host.
	CommandTimeoutSeconds int `toml:"command_timeout_seconds"`
	// ProbeTimeoutSeconds bounds one hostctl status probe during placement.
	// The whole probe pass runs in parallel, so it is bounded by this value,
	// not by the number of hosts.
	ProbeTimeoutSeconds int `toml:"probe_timeout_seconds"`
	// SlotsPerHost is an informational planning hint for how many concurrent
	// guests each host should run. Placement reads the host's reported
	// Capacity.Total, which tart-hostctl derives from its own
	// TART_FLEET_SLOTS_PER_HOST (see internal/hostctl), so this field does not
	// itself enforce concurrency. It is still validated against the licence
	// cap so a misconfigured plan is caught at startup.
	SlotsPerHost int `toml:"slots_per_host"`

	// Image is the golden image to clone when the bootstrap payload does not
	// carry one.
	Image string `toml:"image"`
	// DefaultCPU, DefaultMemoryMB and DefaultDiskGB are the tart set arguments
	// used when the bootstrap extra_specs do not override them.
	DefaultCPU      int `toml:"default_cpu"`
	DefaultMemoryMB int `toml:"default_memory_mb"`
	DefaultDiskGB   int `toml:"default_disk_gb"`

	// Hosts is the fleet this provider manages.
	Hosts []Host `toml:"hosts"`
}

// LoadConfig reads and validates the provider config file.
func LoadConfig(path string) (*Config, error) {
	if path == "" {
		return nil, fmt.Errorf("GARM_PROVIDER_CONFIG_FILE is not set")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %q: %w", path, err)
	}
	cfg := &Config{}
	if err := toml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse config %q: %w", path, err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid config %q: %w", path, err)
	}
	return cfg, nil
}

// Validate fills defaults and rejects configs the provider cannot honour.
func (c *Config) Validate() error {
	if len(c.Hosts) == 0 {
		return fmt.Errorf("no hosts configured")
	}
	if c.SSHUser == "" {
		return fmt.Errorf("ssh_user is required")
	}
	if c.SlotsPerHost == 0 {
		c.SlotsPerHost = 1
	}
	if c.SlotsPerHost > maxSlotsPerHost {
		return fmt.Errorf("slots_per_host %d exceeds the macOS licence cap of %d guests per host", c.SlotsPerHost, maxSlotsPerHost)
	}
	if c.CreateTimeoutSeconds == 0 {
		c.CreateTimeoutSeconds = defaultCreateTimeoutSeconds
	}
	if c.CommandTimeoutSeconds == 0 {
		c.CommandTimeoutSeconds = defaultCommandTimeoutSecs
	}
	if c.ProbeTimeoutSeconds == 0 {
		c.ProbeTimeoutSeconds = defaultProbeTimeoutSeconds
	}
	if c.HostctlBin == "" {
		c.HostctlBin = defaultHostctlBin
	}
	if c.DefaultCPU == 0 {
		c.DefaultCPU = defaultCPU
	}
	if c.DefaultMemoryMB == 0 {
		c.DefaultMemoryMB = defaultMemoryMB
	}
	if c.DefaultDiskGB == 0 {
		c.DefaultDiskGB = defaultDiskGB
	}
	seen := make(map[string]bool, len(c.Hosts))
	for _, host := range c.Hosts {
		if host.Name == "" || host.Address == "" {
			return fmt.Errorf("host entries need both name and address")
		}
		if seen[host.Name] {
			return fmt.Errorf("duplicate host name %q", host.Name)
		}
		seen[host.Name] = true
	}
	return nil
}

// HostByName returns the configured host called name.
func (c *Config) HostByName(name string) (Host, bool) {
	for _, host := range c.Hosts {
		if host.Name == name {
			return host, true
		}
	}
	return Host{}, false
}

// CommandTimeout is the bound for one SSH command.
func (c *Config) CommandTimeout() time.Duration {
	return time.Duration(c.CommandTimeoutSeconds) * time.Second
}

// ProbeTimeout is the bound for one host status probe during placement.
func (c *Config) ProbeTimeout() time.Duration {
	return time.Duration(c.ProbeTimeoutSeconds) * time.Second
}

// CreateTimeout is the end-to-end bound for one create call.
func (c *Config) CreateTimeout() time.Duration {
	return time.Duration(c.CreateTimeoutSeconds) * time.Second
}
