package hostctl

import (
	"context"
	"os"
	"time"
)

// Reconcile brings the registry into line with `tart list`. It runs at startup
// and on every status (and before a create reserves capacity). It:
//
//   - drops records whose VM has vanished from tart list (someone ran
//     `tart delete` by hand) — except deleting tombstones, which the delete
//     verb itself completes;
//   - advances or reaps records left in `cloning`/`running` by a crashed create
//     once they are older than the stale-create TTL;
//   - adopts unrecorded VMs that carry this controller's label (they keep a
//     lease and count against capacity);
//   - quarantines all other unrecorded VMs without deleting them, and counts
//     them against capacity until an operator resolves ownership.
//
// A provider killed between `tart clone` and the record write leaves a VM with
// no record here: the next Reconcile adopts or quarantines it. Tart does not
// expose owner labels in current releases, so an unrecognised VM must never be
// deleted automatically: it might belong to a person or another controller.
func (s *Service) Reconcile() error {
	vms, err := s.tart.List()
	if err != nil {
		return err
	}
	byName := make(map[string]VM, len(vms))
	for _, vm := range vms {
		byName[vm.Name] = vm
	}

	records, err := s.reg.All()
	if err != nil {
		return err
	}
	recByName := make(map[string]Record, len(records))
	for _, rec := range records {
		recByName[rec.Name] = rec
	}

	now := s.now()

	// 1. Drop records whose VM is gone and that are not deleting tombstones.
	for _, rec := range records {
		if _, ok := byName[rec.Name]; ok {
			continue
		}
		if rec.State == StateDeleting {
			continue
		}
		_ = s.reg.Delete(rec.Name)
	}

	// 2. Reclaim stale creates: a record stuck in cloning/running past the
	// stale-create TTL means a create was interrupted (crash). A running record
	// is re-probed and advanced to ready when the guest now answers; otherwise,
	// and for a cloning record (which cannot be resumed), the VM is deleted and
	// the record dropped, releasing the slot.
	s.reclaimStaleCreates(records, byName, now)

	// 3. Adopt or quarantine unrecorded VMs. Unknown ownership is never proof
	// that a VM may be deleted, even when it carries a foreign controller label.
	for _, vm := range vms {
		if _, ok := recByName[vm.Name]; ok {
			continue
		}
		switch label := vm.Label(controllerLabelKey); {
		case label != "" && label == s.controllerID:
			rec := Record{
				Name:         vm.Name,
				ProviderID:   s.hostName + "/" + vm.Name,
				ControllerID: s.controllerID,
				State:        StateAdopted,
				Lease: Lease{
					Holder:     s.controllerID,
					AcquiredAt: now,
					TTLSec:     defaultLeaseTTLSeconds,
				},
				CreatedAt: now,
				UpdatedAt: now,
			}
			if err := s.reg.Save(rec); err != nil {
				return err
			}
		default:
			quarantinedAt := now
			rec := Record{
				Name:          vm.Name,
				State:         StateQuarantined,
				QuarantinedAt: &quarantinedAt,
				CreatedAt:     now,
				UpdatedAt:     now,
			}
			if err := s.reg.Save(rec); err != nil {
				return err
			}
		}
	}
	return nil
}

// reclaimStaleCreates advances or reaps records left in cloning/running by an
// interrupted create. A record in those states whose UpdatedAt is older than
// the stale-create TTL is either resumed (a running record re-probed with
// `tart exec true` and advanced to ready when the guest now answers) or reaped
// (the VM deleted and the record dropped, releasing the slot). Records younger
// than the TTL are left alone; a record whose VM already vanished is handled by
// the drop-vanished step, so it is skipped here.
func (s *Service) reclaimStaleCreates(records []Record, byName map[string]VM, now time.Time) {
	for _, rec := range records {
		if rec.State != StateCloning && rec.State != StateRunning {
			continue
		}
		if now.Sub(rec.UpdatedAt) < s.staleCreateTTL {
			continue
		}
		if _, ok := byName[rec.Name]; !ok {
			continue // vanished; the drop-vanished step already removed it
		}
		if rec.State == StateRunning && s.resumeRunning(rec) {
			continue
		}
		s.reapStuck(rec)
	}
}

// resumeRunning re-probes readiness for a stale running record and, when the
// guest now answers, advances it to ready (running -> ready). It reports
// whether the record was advanced. The probe is bounded by the host so a guest
// that still never answers cannot pin the exclusive flock held by Reconcile.
func (s *Service) resumeRunning(rec Record) bool {
	probeCtx, cancel := context.WithTimeout(context.Background(), s.probeTimeout)
	defer cancel()
	if err := s.tart.Exec(probeCtx, rec.Name, nil, "true"); err != nil {
		return false
	}
	rec.State = StateReady
	rec.LastError = ""
	rec.UpdatedAt = s.now()
	if err := s.reg.Save(rec); err != nil {
		return false
	}
	return true
}

// reapStuck deletes a stuck VM and drops its record, releasing the slot. A
// failed delete keeps the record so the next reconcile retries.
func (s *Service) reapStuck(rec Record) {
	_ = s.tart.Stop(rec.Name) // bounded by the transport timeout; best-effort
	if err := s.tart.Delete(rec.Name); err != nil {
		return
	}
	// A successful exit is not evidence of deletion. Preserve the record if
	// Tart still reports the VM, or if the verification command fails.
	present, err := s.tartPresence(rec.Name)
	if err != nil || present {
		return
	}
	_ = s.reg.Delete(rec.Name)
}

// hostname returns the machine's host name, used as the fallback host component
// of provider IDs when no TART_FLEET_HOST override is configured.
func hostname() string {
	h, err := os.Hostname()
	if err != nil {
		return "unknown-host"
	}
	return h
}
