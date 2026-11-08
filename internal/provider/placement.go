package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/yanksyoon/garm-provider-tart/internal/hostctl"
)

// FaultNoReachableHost is the provider-side placement fault returned when every
// host is unreachable (timeout, auth failure or bad JSON). It is distinct from
// hostctl's capacity fault so GARM and operators can tell "nothing reachable"
// from "everything full".
const FaultNoReachableHost = "no_reachable_host"

// defaultWaitMargin is how much of the create timeout is reserved for the
// create itself after a capacity wait: the provider stops waiting for a free
// slot at create_timeout − defaultWaitMargin, leaving room to clone and boot.
const defaultWaitMargin = 120 * time.Second

// jitterInterval returns a 5–10 s re-probe delay with jitter, as the capacity
// wait requires.
func jitterInterval() time.Duration {
	return 5*time.Second + time.Duration(rand.Int63n(int64(5*time.Second)+1))
}

// hostClass is the tri-state a parallel probe reduces each host to.
type hostClass int

const (
	classUp hostClass = iota
	classFull
	classUnknown
)

// hostProbe is the classified result of probing one host.
type hostProbe struct {
	host     Host
	class    hostClass
	status   *hostctl.Status
	free     int
	lastUsed time.Time
	err      error
}

// hostResult is one fan-out outcome (raw stdout plus error) for a host.
type hostResult struct {
	host Host
	out  []byte
	err  error
}

// probeHosts probes every host in parallel, each bounded by probe_timeout_seconds,
// and classifies each as up / full / unknown. The whole pass is bounded by the
// per-host timeout, not by N × timeout.
func (p *Provider) probeHosts(ctx context.Context) []hostProbe {
	results := p.fanOut(ctx, p.cfg.Hosts, p.cfg.ProbeTimeout(), "status")
	probes := make([]hostProbe, len(results))
	for i, r := range results {
		probes[i] = classifyProbe(r)
	}
	return probes
}

// fanOut runs one hostctl verb on every host concurrently and returns each
// host's raw result.
func (p *Provider) fanOut(ctx context.Context, hosts []Host, perHost time.Duration, verb string, args ...string) []hostResult {
	results := make([]hostResult, len(hosts))
	var wg sync.WaitGroup
	for i, h := range hosts {
		wg.Add(1)
		go func(i int, h Host) {
			defer wg.Done()
			hctx, cancel := context.WithTimeout(ctx, perHost)
			defer cancel()
			out, err := p.client(h).Run(hctx, verb, args...)
			results[i] = hostResult{host: h, out: out, err: err}
		}(i, h)
	}
	wg.Wait()
	return results
}

// classifyProbe reduces a raw status result to a hostProbe. A host that answers
// but is drained, keychain-locked or full is `full` (not placeable); a host that
// cannot be reached or whose status is unreadable is `unknown`.
func classifyProbe(r hostResult) hostProbe {
	hp := hostProbe{host: r.host}
	if r.err != nil {
		hp.class = classUnknown
		hp.err = r.err
		return hp
	}
	var st hostctl.Status
	if err := json.Unmarshal(r.out, &st); err != nil {
		hp.class = classUnknown
		hp.err = fmt.Errorf("unreadable status from %s: %w", r.host.Name, err)
		return hp
	}
	hp.status = &st
	used := st.Capacity.Used + st.Capacity.Adopted + st.Capacity.Quarantined
	hp.free = st.Capacity.Total - used
	if hp.free < 0 {
		hp.free = 0
	}
	hp.lastUsed = lastActivity(st.Instances)
	switch {
	case st.Drained:
		hp.class = classFull
	case !st.KeychainUnlocked:
		hp.class = classFull
	case hp.free == 0:
		hp.class = classFull
	default:
		hp.class = classUp
	}
	return hp
}

// lastActivity returns the most recent record timestamp on a host, or the zero
// time for an idle host (which sorts first, as least recently used).
func lastActivity(instances []hostctl.Instance) time.Time {
	var latest time.Time
	for _, inst := range instances {
		if inst.UpdatedAt.After(latest) {
			latest = inst.UpdatedAt
		}
	}
	return latest
}

// placementDecision tells the create loop what to do next.
type placementDecision int

const (
	decisionPlace        placementDecision = iota
	decisionWaitCapacity                   // no up host, but at least one full host
	decisionNoReachable                    // every host is unknown
)

// decide picks a host from the probe results, honouring a pinned host first.
// With no pin it prefers the least-used up host (most free slots), then the
// least recently used, then name order for determinism.
func (p *Provider) decide(pinned string, probes []hostProbe) (Host, placementDecision, error) {
	if pinned != "" {
		for _, hp := range probes {
			if hp.host.Name == pinned {
				switch hp.class {
				case classUp:
					return hp.host, decisionPlace, nil
				case classFull:
					return Host{}, decisionWaitCapacity, nil
				default:
					return Host{}, decisionNoReachable, nil
				}
			}
		}
		return Host{}, decisionPlace, fmt.Errorf("extra_specs.host %q is not a configured host", pinned)
	}

	var ups []hostProbe
	fulls := 0
	for _, hp := range probes {
		switch hp.class {
		case classUp:
			ups = append(ups, hp)
		case classFull:
			fulls++
		}
	}
	if len(ups) > 0 {
		sort.SliceStable(ups, func(i, j int) bool {
			if ups[i].free != ups[j].free {
				return ups[i].free > ups[j].free
			}
			if !ups[i].lastUsed.Equal(ups[j].lastUsed) {
				return ups[i].lastUsed.Before(ups[j].lastUsed)
			}
			return ups[i].host.Name < ups[j].host.Name
		})
		return ups[0].host, decisionPlace, nil
	}
	if fulls > 0 {
		return Host{}, decisionWaitCapacity, nil
	}
	return Host{}, decisionNoReachable, nil
}

// placeAndCreate runs the placement loop and, once a host is chosen, drives the
// hostctl create verb. It returns the created instance, or a classified fault
// (no_reachable_host / capacity) that the caller turns into an errored
// ProviderInstance, or a hard transport/tart error GARM must retry.
func (p *Provider) placeAndCreate(ctx context.Context, name, image string, specs extraSpecs, owner string) (hostctl.Instance, Host, error) {
	deadline := time.Now().Add(p.waitBudget())
	for {
		probes := p.probeHosts(ctx)
		p.logProbe(probes)

		host, decision, err := p.decide(specs.Host, probes)
		if err != nil {
			return hostctl.Instance{}, Host{}, err
		}

		switch decision {
		case decisionNoReachable:
			rep := p.representativeHost(specs.Host)
			return hostctl.Instance{}, rep, &FaultError{
				Fault:   FaultNoReachableHost,
				Message: fmt.Sprintf("no host reachable: all %d host(s) are unknown", len(probes)),
			}

		case decisionWaitCapacity:
			if !time.Now().Before(deadline) {
				rep := p.representativeHost(specs.Host)
				return hostctl.Instance{}, rep, &FaultError{
					Fault:   hostctl.FaultCapacity,
					Message: fmt.Sprintf("no free slot on any host (waited %s)", p.waitBudget()),
				}
			}
			if err := p.sleepInterval(ctx); err != nil {
				return hostctl.Instance{}, Host{}, err
			}

		case decisionPlace:
			inst, err := p.createOn(ctx, host, name, image, specs, owner)
			if err == nil {
				return inst, host, nil
			}
			var fe *FaultError
			if errors.As(err, &fe) && fe.Fault == hostctl.FaultCapacity {
				// Lost a race for the last slot (double-book): re-probe and
				// retry on another host, within the deadline.
				continue
			}
			// Any other failure is hard: clean up whatever the attempt may have
			// created, then surface it so GARM retries.
			p.cleanup(host, name)
			return hostctl.Instance{}, host, err
		}
	}
}

// createOn runs the hostctl create verb on one host and parses the result.
func (p *Provider) createOn(ctx context.Context, host Host, name, image string, specs extraSpecs, owner string) (hostctl.Instance, error) {
	out, err := p.client(host).Run(ctx, "create",
		"--name", name,
		"--image", image,
		"--cpu", strconv.Itoa(specs.cpu(p.cfg)),
		"--mem", strconv.Itoa(specs.mem(p.cfg)),
		"--disk", strconv.Itoa(specs.disk(p.cfg)),
		"--controller", p.controllerID,
		"--owner", owner,
		"--host", host.Name,
	)
	if err != nil {
		return hostctl.Instance{}, err
	}
	var inst hostctl.Instance
	if err := json.Unmarshal(out, &inst); err != nil {
		return hostctl.Instance{}, fmt.Errorf("parse hostctl create output from %s: %w", host.Name, err)
	}
	return inst, nil
}

// cleanup best-effort deletes a VM a failed create attempt may have left
// behind. The host-side reaper and adoption handle the case where the host is
// unreachable and this delete cannot run.
func (p *Provider) cleanup(host Host, name string) {
	cctx, cancel := context.WithTimeout(context.Background(), p.cfg.CommandTimeout())
	defer cancel()
	if _, err := p.client(host).Run(cctx, "delete", "--name", name); err != nil {
		p.logf("cleanup: delete %q on %s failed: %v (host-side reaper will finish it)", name, host.Name, err)
	}
}

// waitBudget is how long a create may wait for a free slot before failing with
// capacity: the create timeout minus the reserved create margin.
func (p *Provider) waitBudget() time.Duration {
	b := p.cfg.CreateTimeout() - p.waitMargin
	if b < 0 {
		return 0
	}
	return b
}

// representativeHost picks a host to name in a placement-fault provider id.
func (p *Provider) representativeHost(pinned string) Host {
	if pinned != "" {
		if h, ok := p.cfg.HostByName(pinned); ok {
			return h
		}
	}
	return p.cfg.Hosts[0]
}

// sleepInterval waits for one re-probe interval, or until ctx is done.
func (p *Provider) sleepInterval(ctx context.Context) error {
	timer := time.NewTimer(p.probeInterval())
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// logProbe emits the placement metrics for one probe round: the up/full/unknown
// split, per-host capacity, and the unknown-host count.
func (p *Provider) logProbe(probes []hostProbe) {
	var up, full, unknown int
	for _, hp := range probes {
		switch hp.class {
		case classUp:
			up++
		case classFull:
			full++
		case classUnknown:
			unknown++
		}
	}
	p.metric("probe up=%d full=%d unknown=%d", up, full, unknown)
	for _, hp := range probes {
		if hp.status != nil {
			p.metric("capacity host=%s total=%d used=%d adopted=%d quarantined=%d free=%d",
				hp.host.Name, hp.status.Capacity.Total, hp.status.Capacity.Used,
				hp.status.Capacity.Adopted, hp.status.Capacity.Quarantined, hp.free)
		}
	}
	if unknown > 0 {
		p.metric("unknown_hosts=%d", unknown)
	}
}
