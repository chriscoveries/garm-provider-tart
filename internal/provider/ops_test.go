package provider

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	commonParams "github.com/cloudbase/garm-provider-common/params"

	"github.com/yanksyoon/garm-provider-tart/internal/hostctl"
)

func TestCreateInstanceStreamsRunnerBootstrapToGuest(t *testing.T) {
	const script = "#!/bin/sh\necho install runner\n"
	bootstrapOutput := func(host, verb string, args []string) ([]byte, error) {
		switch verb {
		case "status":
			return statusJSON(t, nil), nil
		case "create":
			return instanceJSON(t, host, "garm-abc123", hostctl.StateReady), nil
		case "bootstrap":
			return instanceJSON(t, host, "garm-abc123", hostctl.StateIdle), nil
		}
		return nil, nil
	}
	p, fleet, _ := newFleetProvider(t, []Host{{Name: "mac-mini-01", Address: "10.20.0.11"}}, bootstrapOutput)
	extras, _ := json.Marshal(map[string]string{"runner_install_template": script})
	var stdout strings.Builder
	if err := p.CreateInstance(strings.NewReader(bootstrapJSON(t, "garm-abc123", extras)), &stdout); err != nil {
		t.Fatal(err)
	}
	if len(fleet.bootstrapScripts) != 1 || fleet.bootstrapScripts[0] != script {
		t.Fatalf("bootstrap stdin = %#v, want the exact runner wrapper", fleet.bootstrapScripts)
	}
	if got := fleet.callsFor("mac-mini-01", "bootstrap"); len(got) != 1 || got[0] != "--name garm-abc123" {
		t.Fatalf("bootstrap calls = %v", got)
	}
	got := decodeInstance(t, stdout.String())
	if got.Status != commonParams.InstanceRunning {
		t.Fatalf("status = %q, want running after bootstrap", got.Status)
	}
}

func TestCreateInstanceBootstrapFailureCleansUp(t *testing.T) {
	handler := func(host, verb string, args []string) ([]byte, error) {
		switch verb {
		case "status":
			return statusJSON(t, nil), nil
		case "create":
			return instanceJSON(t, host, "garm-abc123", hostctl.StateReady), nil
		case "bootstrap":
			return nil, errors.New("guest runner install failed")
		}
		return nil, nil
	}
	p, fleet, _ := newFleetProvider(t, []Host{{Name: "mac-mini-01", Address: "10.20.0.11"}}, handler)
	extras := json.RawMessage(`{"runner_install_template":"#!/bin/sh\\nexit 1\\n"}`)
	var stdout strings.Builder
	err := p.CreateInstance(strings.NewReader(bootstrapJSON(t, "garm-abc123", extras)), &stdout)
	if err == nil || !strings.Contains(err.Error(), "guest runner install failed") {
		t.Fatalf("create error = %v, want bootstrap failure", err)
	}
	if got := fleet.callsFor("mac-mini-01", "delete"); len(got) != 1 {
		t.Fatalf("cleanup delete calls = %v, want one", got)
	}
	if stdout.Len() != 0 {
		t.Fatalf("success payload emitted after bootstrap failure: %q", stdout.String())
	}
}

func TestCreateInstanceRejectsBootstrapWithoutIdleState(t *testing.T) {
	handler := func(host, verb string, args []string) ([]byte, error) {
		switch verb {
		case "status":
			return statusJSON(t, nil), nil
		case "create", "bootstrap":
			return instanceJSON(t, host, "garm-abc123", hostctl.StateReady), nil
		}
		return nil, nil
	}
	p, fleet, _ := newFleetProvider(t, []Host{{Name: "mac-mini-01", Address: "10.20.0.11"}}, handler)
	var stdout strings.Builder
	err := p.CreateInstance(strings.NewReader(bootstrapJSON(t, "garm-abc123", json.RawMessage(`{"runner_install_template":"echo hello"}`))), &stdout)
	if err == nil || !strings.Contains(err.Error(), "unexpected instance state") {
		t.Fatalf("create error = %v, want non-idle bootstrap rejection", err)
	}
	if got := fleet.callsFor("mac-mini-01", "delete"); len(got) != 1 {
		t.Fatalf("cleanup delete calls = %v, want one", got)
	}
}

// TestMapStatus locks the ready-vs-running contract (design 5.2): only the
// post-bootstrap states map to GARM `running`, while a `running` record — a VM
// that reached Tart's running state but whose readiness probe has not passed —
// maps to `error` so GARM never marks a half-bootstrapped VM healthy.
func TestMapStatus(t *testing.T) {
	// Arrange: every hostctl state and its expected GARM status.
	cases := []struct {
		name  string
		state hostctl.State
		want  commonParams.InstanceStatus
	}{
		{"ready", hostctl.StateReady, commonParams.InstanceRunning},
		{"bootstrapping", hostctl.StateBootstrapping, commonParams.InstanceRunning},
		{"idle", hostctl.StateIdle, commonParams.InstanceRunning},
		{"adopted", hostctl.StateAdopted, commonParams.InstanceRunning},
		{"cloning", hostctl.StateCloning, commonParams.InstanceCreating},
		{"running", hostctl.StateRunning, commonParams.InstanceError},
		{"failed", hostctl.StateFailed, commonParams.InstanceError},
		{"deleting", hostctl.StateDeleting, commonParams.InstanceDeleting},
		{"quarantined", hostctl.StateQuarantined, commonParams.InstanceStatusUnknown},
		{"gone", hostctl.StateGone, commonParams.InstanceStatusUnknown},
		{"unknown", hostctl.State("bogus"), commonParams.InstanceStatusUnknown},
	}
	for _, tc := range cases {
		// Act.
		got := mapStatus(tc.state)

		// Assert.
		if got != tc.want {
			t.Errorf("mapStatus(%q) = %q, want %q", tc.state, got, tc.want)
		}
	}
}

// TestProviderInstanceRunningWithLastErrorReportsFault covers the
// StateRunning && LastError != "" branch: a VM that reached running but whose
// readiness probe failed must surface as an errored instance whose
// provider_fault carries the last_error, not as a healthy running instance.
func TestProviderInstanceRunningWithLastErrorReportsFault(t *testing.T) {
	// Arrange: a running record whose readiness probe failed.
	inst := hostctl.Instance{
		Record: hostctl.Record{
			Name:       "garm-abc123",
			ProviderID: "mac-mini-01/garm-abc123",
			State:      hostctl.StateRunning,
			LastError:  "readiness probe timed out",
		},
	}
	p := New(&Config{}, "controller-1")

	// Act.
	pi := p.providerInstance(inst)

	// Assert: error status, and the fault names the last_error.
	if pi.Status != commonParams.InstanceError {
		t.Fatalf("status = %q, want error", pi.Status)
	}
	if string(pi.ProviderFault) != "readiness probe timed out" {
		t.Fatalf("provider_fault = %q, want readiness probe timed out", string(pi.ProviderFault))
	}
}

// TestProviderInstanceRunningWithoutErrorStillError covers the else side of the
// fault branch: a running record with no last_error is still an error (the slot
// is held but the VM is not yet ready), so GARM must not treat it as healthy.
func TestProviderInstanceRunningWithoutErrorStillError(t *testing.T) {
	// Arrange: a running record with an empty last_error.
	inst := hostctl.Instance{
		Record: hostctl.Record{
			Name:       "garm-abc123",
			ProviderID: "mac-mini-01/garm-abc123",
			State:      hostctl.StateRunning,
		},
	}
	p := New(&Config{}, "controller-1")

	// Act.
	pi := p.providerInstance(inst)

	// Assert: error status, and no fault string.
	if pi.Status != commonParams.InstanceError {
		t.Fatalf("status = %q, want error", pi.Status)
	}
	if len(pi.ProviderFault) != 0 {
		t.Fatalf("provider_fault = %q, want empty", string(pi.ProviderFault))
	}
}

// TestProviderInstanceReadyReportsRunning is the positive half of the same
// contract: the only state GARM may see as running is a record that actually
// passed readiness, with no fault even if a stale last_error lingered.
func TestProviderInstanceReadyReportsRunning(t *testing.T) {
	// Arrange: a ready record.
	inst := hostctl.Instance{
		Record: hostctl.Record{
			Name:       "garm-abc123",
			ProviderID: "mac-mini-01/garm-abc123",
			State:      hostctl.StateReady,
		},
	}
	p := New(&Config{}, "controller-1")

	// Act.
	pi := p.providerInstance(inst)

	// Assert: running, no fault.
	if pi.Status != commonParams.InstanceRunning {
		t.Fatalf("status = %q, want running", pi.Status)
	}
	if len(pi.ProviderFault) != 0 {
		t.Fatalf("provider_fault = %q, want empty", string(pi.ProviderFault))
	}
}
