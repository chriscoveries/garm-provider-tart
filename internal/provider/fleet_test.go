package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yanksyoon/garm-provider-tart/internal/hostctl"
)

// fleetFake is an in-memory fleet: a handler that returns canned stdout/error
// per (host, verb, args) call, plus a call log for assertions.
type fleetFake struct {
	mu               sync.Mutex
	handler          func(host, verb string, args []string) ([]byte, error)
	calls            []string
	bootstrapScripts []string
}

func (f *fleetFake) run(host, verb string, args []string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, host+" "+verb+" "+strings.Join(args, " "))
	if f.handler != nil {
		return f.handler(host, verb, args)
	}
	return nil, nil
}

// callsFor returns the argument strings of every call of verb on host.
func (f *fleetFake) callsFor(host, verb string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		parts := strings.SplitN(c, " ", 3)
		if len(parts) == 3 && parts[0] == host && parts[1] == verb {
			out = append(out, parts[2])
		}
	}
	return out
}

// fakeRunner binds a fleetFake to one host, implementing runner.
type fakeRunner struct {
	fleet *fleetFake
	host  string
}

func (r *fakeRunner) Run(ctx context.Context, verb string, args ...string) ([]byte, error) {
	return r.fleet.run(r.host, verb, args)
}

func (r *fakeRunner) RunInput(ctx context.Context, stdin io.Reader, verb string, args ...string) ([]byte, error) {
	data, err := io.ReadAll(stdin)
	if err != nil {
		return nil, err
	}
	r.fleet.mu.Lock()
	r.fleet.bootstrapScripts = append(r.fleet.bootstrapScripts, string(data))
	r.fleet.mu.Unlock()
	return r.fleet.run(r.host, verb, args)
}

// newFleetProvider wires a provider to an in-memory fleet with fast timing
// knobs so placement waits resolve in milliseconds.
func newFleetProvider(t *testing.T, hosts []Host, handler func(host, verb string, args []string) ([]byte, error)) (*Provider, *fleetFake, *bytes.Buffer) {
	t.Helper()
	f := &fleetFake{handler: handler}
	cfg := &Config{
		Hosts:                 hosts,
		SSHUser:               "garm",
		CreateTimeoutSeconds:  900,
		CommandTimeoutSeconds: 30,
		ProbeTimeoutSeconds:   3,
		SlotsPerHost:          2,
		Image:                 "ghcr.io/cirruslabs/macos-tahoe-xcode@sha256:abc",
		DefaultCPU:            4,
		DefaultMemoryMB:       8192,
		DefaultDiskGB:         50,
	}
	p := New(cfg, "controller-1")
	p.clientFor = func(h Host) runner { return &fakeRunner{fleet: f, host: h.Name} }
	var stderr bytes.Buffer
	p.stderr = &stderr
	p.waitMargin = 0
	p.probeInterval = func() time.Duration { return time.Millisecond }
	return p, f, &stderr
}

// threeHosts is a three-host fleet, two slots each.
func threeHosts() []Host {
	return []Host{
		{Name: "mac-mini-01", Address: "10.20.0.11"},
		{Name: "mac-mini-02", Address: "10.20.0.12"},
		{Name: "mac-mini-03", Address: "10.20.0.13"},
	}
}

// statusJSON marshals a hostctl Status, with a mutation hook for the test.
func statusJSON(t *testing.T, mut func(*hostctl.Status)) []byte {
	t.Helper()
	st := hostctl.Status{
		TartVersion:      "tart 2.0.0",
		KeychainUnlocked: true,
		FreeDiskBytes:    100 << 30,
		Capacity:         hostctl.Capacity{Total: 2},
	}
	if mut != nil {
		mut(&st)
	}
	b, err := json.Marshal(st)
	if err != nil {
		t.Fatalf("marshal status: %v", err)
	}
	return b
}

// instanceJSON marshals one hostctl Instance in the given state.
func instanceJSON(t *testing.T, host, name string, state hostctl.State) []byte {
	t.Helper()
	inst := hostctl.Instance{
		Record: hostctl.Record{
			Name:         name,
			ProviderID:   host + "/" + name,
			ControllerID: "controller-1",
			OwnerID:      "pool-1",
			Image:        "ghcr.io/cirruslabs/macos-tahoe-xcode@sha256:abc",
			State:        state,
		},
		IP:     "192.168.64.5",
		InTart: true,
	}
	b, err := json.Marshal(inst)
	if err != nil {
		t.Fatalf("marshal instance: %v", err)
	}
	return b
}

// listJSON marshals a slice of instances.
func listJSON(t *testing.T, instances ...hostctl.Instance) []byte {
	t.Helper()
	b, err := json.Marshal(instances)
	if err != nil {
		t.Fatalf("marshal list: %v", err)
	}
	return b
}

// upStatus returns a status handler snippet with the given free-slot count.
func upStatus(used int) []byte {
	st := hostctl.Status{
		TartVersion:      "tart 2.0.0",
		KeychainUnlocked: true,
		Capacity:         hostctl.Capacity{Total: 2, Used: used},
	}
	b, _ := json.Marshal(st)
	return b
}

// fullStatus returns a status handler snippet with no free slots.
func fullStatus() []byte {
	return upStatus(2)
}
