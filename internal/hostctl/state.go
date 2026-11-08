package hostctl

import "regexp"

// State is one lifecycle position of a tracked instance. The legal transitions
// are the state machine from the architecture decision record:
//
//	create:  cloning → running → ready → bootstrapping → idle
//	delete:  deleting → gone
//	failure: any live state → failed (last_error kept, slot still held)
//
// plus two boot-reconciliation outcomes: adopted (a VM found in `tart list`
// that carries this controller's label) and quarantined (a VM without proven
// ownership, preserved until an operator resolves it).
type State string

const (
	StateCloning       State = "cloning"
	StateRunning       State = "running"
	StateReady         State = "ready"
	StateBootstrapping State = "bootstrapping"
	StateIdle          State = "idle"
	StateDeleting      State = "deleting"
	StateFailed        State = "failed"
	StateAdopted       State = "adopted"
	StateQuarantined   State = "quarantined"

	// StateGone is never persisted. It describes a record that has been removed
	// and a VM that is confirmed absent from `tart list`.
	StateGone State = "gone"
)

// legalTransitions maps each state to the states it may advance directly into.
// `deleting` is deliberately omitted from the per-state lists and handled in
// CanTransition: a tracked VM may be deleted from any live state.
var legalTransitions = map[State][]State{
	StateCloning:       {StateRunning, StateFailed},
	StateRunning:       {StateReady, StateFailed},
	StateReady:         {StateBootstrapping, StateIdle, StateFailed},
	StateBootstrapping: {StateIdle, StateFailed},
	StateIdle:          {StateFailed},
	StateFailed:        {},
	StateAdopted:       {StateRunning, StateReady, StateFailed},
	StateQuarantined:   {},
	StateDeleting:      {StateGone},
	StateGone:          {},
}

// CanTransition reports whether from may move directly to to.
func CanTransition(from, to State) bool {
	if to == StateDeleting && from != StateGone && from != StateDeleting {
		return true
	}
	for _, next := range legalTransitions[from] {
		if next == to {
			return true
		}
	}
	return false
}

// IsLive reports whether the state still occupies a licence slot on the host.
// Every state except gone counts, including deleting tombstones, adopted and
// quarantined VMs: the underlying VM may still exist until it is confirmed
// absent from `tart list`, so releasing the slot earlier would double-book it.
func (s State) IsLive() bool {
	return s != StateGone
}

// namePattern is the only shape a VM name may take before any Tart command is
// touched: a lowercase alphanumeric lead, then lowercase alphanumerics or
// dashes, up to 51 characters total. It is deliberately narrow so a name can
// never be interpreted by a shell or by Tart's flag parser.
var namePattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,50})$`)

// ValidName reports whether name is safe to pass to Tart.
func ValidName(name string) bool {
	return namePattern.MatchString(name)
}
