package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"

	"github.com/yanksyoon/garm-provider-tart/internal/hostctl"
)

// sshExitUnreachable is the OpenSSH exit code for a connection-level failure
// (255): name resolution, connection refused, host key mismatch or
// authentication failure. The provider maps it to a hard error so GARM retries
// rather than reporting success or a stale instance.
const sshExitUnreachable = 255

var (
	// ErrHostUnreachable reports that the host could not be reached over SSH,
	// so any pending delete or create is unresolved and GARM must retry.
	ErrHostUnreachable = errors.New("ssh: host unreachable")
	// ErrCommandTimeout reports that a hostctl verb did not finish in time.
	ErrCommandTimeout = errors.New("ssh: command timed out")
)

// FaultError is a classified hostctl failure. Fault is one of the hostctl fault
// names (capacity, keychain, tart, ...) and lets the provider distinguish a
// capacity fault (returned to GARM as an errored ProviderInstance) from other
// failures (hard errors GARM retries).
type FaultError struct {
	Fault   string
	Message string
}

func (e *FaultError) Error() string { return e.Message }

// Client runs tart-hostctl verbs on one host by exec'ing the system ssh binary.
// Shelling out to ssh (rather than an in-process SSH client) reuses known_hosts
// and the agent for host-key and credential handling, and keeps the break-glass
// `ssh` command wrapper in authorized_keys as the only remote surface.
type Client struct {
	host       Host
	user       string
	keyPath    string
	knownHosts string
	sshBin     string
	hostctlBin string
}

// NewClient builds a Client for one host.
func NewClient(host Host, user, keyPath, knownHosts, sshBin, hostctlBin string) *Client {
	if sshBin == "" {
		// GARM runs external providers with a minimal environment that has no
		// PATH, so `exec.LookPath("ssh")` cannot resolve the client there. Try
		// PATH first (interactive/tests), then fall back to the standard
		// location on the Linux/macOS controllers this provider targets.
		if p, err := exec.LookPath("ssh"); err == nil {
			sshBin = p
		} else {
			sshBin = "/usr/bin/ssh"
		}
	}
	if hostctlBin == "" {
		hostctlBin = "/usr/local/bin/tart-hostctl"
	}
	return &Client{
		host:       host,
		user:       user,
		keyPath:    keyPath,
		knownHosts: knownHosts,
		sshBin:     sshBin,
		hostctlBin: hostctlBin,
	}
}

// Run executes one hostctl verb and returns its stdout (JSON). Diagnostics and
// structured faults arrive on stderr and are classified into the returned
// error.
func (c *Client) Run(ctx context.Context, verb string, args ...string) ([]byte, error) {
	return c.RunInput(ctx, nil, verb, args...)
}

// RunInput streams sensitive script data to hostctl's stdin. The script is
// never part of the SSH remote command line, fault output, or registry.
func (c *Client) RunInput(ctx context.Context, stdin io.Reader, verb string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, c.sshBin, c.argv(verb, args...)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdin = stdin
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("%w: %s", ErrCommandTimeout, verb)
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			if exitErr.ExitCode() == sshExitUnreachable {
				return nil, fmt.Errorf("%w: %s", ErrHostUnreachable, c.host.Address)
			}
			return nil, classifyFault(stderr.String())
		}
		if ctx.Err() != nil {
			return nil, fmt.Errorf("%w: %s", ErrCommandTimeout, verb)
		}
		return nil, fmt.Errorf("ssh %s: %w", c.host.Address, err)
	}
	return stdout.Bytes(), nil
}

// argv builds the full ssh command: options, target, then the remote hostctl
// invocation. OpenSSH concatenates remote argv into a shell command string;
// quote all dynamic arguments before they cross that shell boundary.
func (c *Client) argv(verb string, args ...string) []string {
	argv := []string{
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout=10",
	}
	if c.keyPath != "" {
		argv = append(argv, "-i", c.keyPath)
	}
	if c.knownHosts != "" {
		argv = append(argv, "-o", "UserKnownHostsFile="+c.knownHosts, "-o", "StrictHostKeyChecking=yes")
	} else {
		// Trust new hosts on first connection, but reject changed keys.
		// Disabling host-key checks exposes the runner script and credentials
		// to an active network attacker.
		argv = append(argv, "-o", "StrictHostKeyChecking=accept-new")
	}
	argv = append(argv, "-p", strconv.Itoa(c.host.port()))
	target := c.host.Address
	if c.user != "" {
		target = c.user + "@" + target
	}
	argv = append(argv, target, c.hostctlBin, verb)
	for _, arg := range args {
		argv = append(argv, quoteRemoteArg(arg))
	}
	return argv
}

// quoteRemoteArg ensures that the remote POSIX shell passes one exact argument
// to hostctl, including spaces, single quotes, and shell metacharacters.
func quoteRemoteArg(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

// classifyFault parses the structured Fault a failed hostctl verb writes to
// stderr, falling back to a generic failure when stderr is not a Fault object.
func classifyFault(stderr string) error {
	trimmed := bytes.TrimSpace([]byte(stderr))
	var f hostctl.Fault
	if err := json.Unmarshal(trimmed, &f); err == nil && f.Fault != "" {
		return &FaultError{Fault: f.Fault, Message: f.Error}
	}
	return fmt.Errorf("hostctl failed: %s", string(trimmed))
}
