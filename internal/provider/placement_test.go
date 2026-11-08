package provider

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	commonParams "github.com/cloudbase/garm-provider-common/params"

	"github.com/yanksyoon/garm-provider-tart/internal/hostctl"
)

func bootstrapJSON(t *testing.T, name string, extraSpecs json.RawMessage) string {
	t.Helper()
	b, err := json.Marshal(commonParams.BootstrapInstance{
		Name:       name,
		PoolID:     "pool-1",
		OSType:     commonParams.Linux,
		OSArch:     commonParams.Arm64,
		ExtraSpecs: extraSpecs,
	})
	if err != nil {
		t.Fatalf("marshal bootstrap: %v", err)
	}
	return string(b)
}

// decodeInstance parses the create output into a ProviderInstance.
func decodeInstance(t *testing.T, out string) commonParams.ProviderInstance {
	t.Helper()
	var pi commonParams.ProviderInstance
	if err := json.Unmarshal([]byte(out), &pi); err != nil {
		t.Fatalf("create output is not one JSON object: %v (%q)", err, out)
	}
	return pi
}

func TestPlacementClassifyUpFullUnknown(t *testing.T) {
	// Arrange: the four raw outcomes a status probe can produce.
	up := hostResult{host: Host{Name: "a"}, out: statusJSON(t, func(st *hostctl.Status) { st.Capacity.Used = 0 })}
	full := hostResult{host: Host{Name: "b"}, out: statusJSON(t, func(st *hostctl.Status) { st.Capacity.Used = 2 })}
	quarantined := hostResult{host: Host{Name: "q"}, out: statusJSON(t, func(st *hostctl.Status) { st.Capacity.Quarantined = 2 })}
	keychain := hostResult{host: Host{Name: "c"}, out: statusJSON(t, func(st *hostctl.Status) { st.KeychainUnlocked = false })}
	drained := hostResult{host: Host{Name: "d"}, out: statusJSON(t, func(st *hostctl.Status) { st.Drained = true })}
	unreachable := hostResult{host: Host{Name: "e"}, err: ErrHostUnreachable}
	corrupt := hostResult{host: Host{Name: "f"}, out: []byte("not json")}

	// Act / Assert: each lands in exactly the expected class.
	cases := []struct {
		name  string
		in    hostResult
		class hostClass
	}{
		{"up", up, classUp},
		{"full", full, classFull},
		{"quarantined-full", quarantined, classFull},
		{"keychain-locked", keychain, classFull},
		{"drained", drained, classFull},
		{"unreachable", unreachable, classUnknown},
		{"corrupt-status", corrupt, classUnknown},
	}
	for _, tc := range cases {
		got := classifyProbe(tc.in)
		if got.class != tc.class {
			t.Fatalf("%s: class = %d, want %d (err=%v)", tc.name, got.class, tc.class, got.err)
		}
	}
}

func TestPlacementLeastUsedThenLRU(t *testing.T) {
	// Arrange: three up hosts; two tie on free slots, one has an older last use.
	older := time.Now().Add(-time.Hour)
	newer := time.Now()
	probes := []hostProbe{
		{host: Host{Name: "a"}, class: classUp, free: 1, lastUsed: older},
		{host: Host{Name: "b"}, class: classUp, free: 2, lastUsed: newer},
		{host: Host{Name: "c"}, class: classUp, free: 2, lastUsed: older},
	}
	p := &Provider{}

	// Act: pick.
	host, decision, err := p.decide("", probes)

	// Assert: least-used wins (free 2), then least recently used (c over b).
	if err != nil {
		t.Fatalf("decide error = %v", err)
	}
	if decision != decisionPlace {
		t.Fatalf("decision = %d, want place", decision)
	}
	if host.Name != "c" {
		t.Fatalf("host = %q, want c (least used, then least recently used)", host.Name)
	}
}

func TestPlacementPinnedHostWins(t *testing.T) {
	// Arrange: three up hosts, but extra_specs.host pins b.
	probes := []hostProbe{
		{host: Host{Name: "a"}, class: classUp, free: 2},
		{host: Host{Name: "b"}, class: classUp, free: 0},
		{host: Host{Name: "c"}, class: classUp, free: 2},
	}
	p := &Provider{}

	// Act: pick with a pin.
	host, decision, err := p.decide("b", probes)

	// Assert: the pin is honoured even though a and c have more room.
	if err != nil {
		t.Fatalf("decide error = %v", err)
	}
	if decision != decisionPlace || host.Name != "b" {
		t.Fatalf("host = %q decision = %d, want b/place", host.Name, decision)
	}
}

func TestPlacementPlacesOnLeastUsedHost(t *testing.T) {
	// Arrange: three hosts, 01 and 03 have one free slot, 02 has two.
	used := map[string]int{"mac-mini-01": 1, "mac-mini-02": 0, "mac-mini-03": 1}
	handler := func(host, verb string, args []string) ([]byte, error) {
		switch verb {
		case "status":
			return statusJSON(t, func(st *hostctl.Status) { st.Capacity.Used = used[host] }), nil
		case "create":
			return instanceJSON(t, host, "garm-abc123", hostctl.StateReady), nil
		}
		return nil, nil
	}
	p, f, _ := newFleetProvider(t, threeHosts(), handler)
	var out strings.Builder

	// Act: create.
	err := p.CreateInstance(strings.NewReader(bootstrapJSON(t, "garm-abc123", nil)), &out)

	// Assert: success, placed on the least-used host.
	if err != nil {
		t.Fatalf("CreateInstance error = %v, want nil", err)
	}
	if got := f.callsFor("mac-mini-02", "create"); len(got) != 1 {
		t.Fatalf("create on mac-mini-02 = %d calls (%v), want 1", len(got), got)
	}
	pi := decodeInstance(t, out.String())
	if pi.ProviderID != "mac-mini-02/garm-abc123" {
		t.Fatalf("provider_id = %q, want mac-mini-02/garm-abc123", pi.ProviderID)
	}
}

func TestPlacementHonoursExtraSpecsHost(t *testing.T) {
	// Arrange: mac-mini-01 has the most free slots, but extra_specs pins 03.
	used := map[string]int{"mac-mini-01": 0, "mac-mini-02": 1, "mac-mini-03": 1}
	handler := func(host, verb string, args []string) ([]byte, error) {
		switch verb {
		case "status":
			return statusJSON(t, func(st *hostctl.Status) { st.Capacity.Used = used[host] }), nil
		case "create":
			return instanceJSON(t, host, "garm-abc123", hostctl.StateReady), nil
		}
		return nil, nil
	}
	p, f, _ := newFleetProvider(t, threeHosts(), handler)
	var out strings.Builder

	// Act: create with a host pin.
	specs := json.RawMessage(`{"host":"mac-mini-03"}`)
	err := p.CreateInstance(strings.NewReader(bootstrapJSON(t, "garm-abc123", specs)), &out)

	// Assert: the pin wins over least-used.
	if err != nil {
		t.Fatalf("CreateInstance error = %v, want nil", err)
	}
	if got := f.callsFor("mac-mini-03", "create"); len(got) != 1 {
		t.Fatalf("create on mac-mini-03 = %d calls, want 1", len(got))
	}
	if got := f.callsFor("mac-mini-01", "create"); len(got) != 0 {
		t.Fatalf("create on mac-mini-01 = %d calls, want 0 (pin ignored)", len(got))
	}
}

func TestPlacementAllUnknownNoReachableHost(t *testing.T) {
	// Arrange: every host is unreachable.
	handler := func(host, verb string, args []string) ([]byte, error) {
		if verb == "status" {
			return nil, ErrHostUnreachable
		}
		return nil, nil
	}
	p, _, _ := newFleetProvider(t, threeHosts(), handler)
	var out strings.Builder

	// Act: create.
	err := p.CreateInstance(strings.NewReader(bootstrapJSON(t, "garm-abc123", nil)), &out)

	// Assert: not a hard error; an errored instance naming no_reachable_host.
	if err != nil {
		t.Fatalf("CreateInstance error = %v, want an errored instance instead", err)
	}
	pi := decodeInstance(t, out.String())
	if pi.Status != commonParams.InstanceError {
		t.Fatalf("status = %q, want error", pi.Status)
	}
	if !strings.Contains(string(pi.ProviderFault), FaultNoReachableHost) {
		t.Fatalf("provider_fault = %q, want no_reachable_host", string(pi.ProviderFault))
	}
}

func TestPlacementAllFullWaitsThenCapacity(t *testing.T) {
	// Arrange: every host full, and a short wait budget that still ends before
	// the create-timeout context expires.
	handler := func(host, verb string, args []string) ([]byte, error) {
		if verb == "status" {
			return fullStatus(), nil
		}
		return nil, nil
	}
	p, _, _ := newFleetProvider(t, threeHosts(), handler)
	p.cfg.CreateTimeoutSeconds = 2
	p.waitMargin = time.Second // wait budget 1 s, leaving 1 s of context
	var out strings.Builder

	// Act: create.
	err := p.CreateInstance(strings.NewReader(bootstrapJSON(t, "garm-abc123", nil)), &out)

	// Assert: capacity fault, distinguishable from no_reachable_host.
	if err != nil {
		t.Fatalf("CreateInstance error = %v, want an errored instance instead", err)
	}
	pi := decodeInstance(t, out.String())
	if pi.Status != commonParams.InstanceError {
		t.Fatalf("status = %q, want error", pi.Status)
	}
	if !strings.Contains(string(pi.ProviderFault), hostctl.FaultCapacity) {
		t.Fatalf("provider_fault = %q, want capacity", string(pi.ProviderFault))
	}
	if strings.Contains(string(pi.ProviderFault), FaultNoReachableHost) {
		t.Fatalf("provider_fault = %q, must not be no_reachable_host", string(pi.ProviderFault))
	}
}

func TestPlacementPartitionedMidCreateHardError(t *testing.T) {
	// Arrange: the host answers the status probe, then stops answering the
	// create (partition mid-create).
	handler := func(host, verb string, args []string) ([]byte, error) {
		switch verb {
		case "status":
			return statusJSON(t, func(st *hostctl.Status) { st.Capacity.Used = 0 }), nil
		case "create":
			return nil, ErrHostUnreachable
		}
		return nil, nil
	}
	p, f, _ := newFleetProvider(t, []Host{{Name: "mac-mini-01", Address: "10.20.0.11"}}, handler)
	var out strings.Builder

	// Act: create.
	err := p.CreateInstance(strings.NewReader(bootstrapJSON(t, "garm-abc123", nil)), &out)

	// Assert: a hard error GARM retries, never a success, and cleanup ran.
	if err == nil {
		t.Fatalf("CreateInstance returned nil, want a hard error")
	}
	if !errors.Is(err, ErrHostUnreachable) {
		t.Fatalf("error = %v, want ErrHostUnreachable", err)
	}
	if out.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", out.String())
	}
	if got := f.callsFor("mac-mini-01", "delete"); len(got) != 1 {
		t.Fatalf("cleanup delete = %d calls, want 1", len(got))
	}
}

func TestPlacementRetriesAfterCapacityFault(t *testing.T) {
	// Arrange: a single host that reports capacity on the first create attempt
	// (a lost race for the last slot), then succeeds on the retry.
	var creates int
	handler := func(host, verb string, args []string) ([]byte, error) {
		switch verb {
		case "status":
			return statusJSON(t, func(st *hostctl.Status) { st.Capacity.Used = 1 }), nil
		case "create":
			creates++
			if creates == 1 {
				return nil, &FaultError{Fault: hostctl.FaultCapacity, Message: "no free slot"}
			}
			return instanceJSON(t, host, "garm-abc123", hostctl.StateReady), nil
		}
		return nil, nil
	}
	p, _, _ := newFleetProvider(t, []Host{{Name: "mac-mini-01", Address: "10.20.0.11"}}, handler)
	var out strings.Builder

	// Act: create.
	err := p.CreateInstance(strings.NewReader(bootstrapJSON(t, "garm-abc123", nil)), &out)

	// Assert: the capacity fault re-probed and retried, and the retry won.
	if err != nil {
		t.Fatalf("CreateInstance error = %v, want nil", err)
	}
	if creates != 2 {
		t.Fatalf("create attempts = %d, want 2", creates)
	}
	pi := decodeInstance(t, out.String())
	if pi.Status != commonParams.InstanceRunning {
		t.Fatalf("status = %q, want running", pi.Status)
	}
	if pi.ProviderID != "mac-mini-01/garm-abc123" {
		t.Fatalf("provider_id = %q, want mac-mini-01/garm-abc123", pi.ProviderID)
	}
}
