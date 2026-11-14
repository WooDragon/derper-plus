// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

package derpserver

import (
	"errors"
	"time"

	"tailscale.com/derp"
)

type coordinatorCounters struct{ attempted, admitted uint64 }
type coordinatorActivity struct {
	subject CoordinatorSubject
	buckets *tailnetRateBuckets
}
type coordinatorState struct {
	started           time.Time
	active            []coordinatorActivity
	accounts, parents int
	invalid           bool
	generation        uint64
	overlay           map[CoordinatorSubject]coordinatorAllocation
}

func (r *tailnetRateRegistry) enableCoordinator() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.coordinator == nil {
		r.coordinator = &coordinatorState{started: r.now(), active: make([]coordinatorActivity, 0, 320), overlay: make(map[CoordinatorSubject]coordinatorAllocation)}
	}
}

func (r *tailnetRateRegistry) countCoordinator(subject CoordinatorSubject, buckets *tailnetRateBuckets, size int, upload, admitted bool) {
	state := r.coordinator
	if state == nil || state.invalid || size == 0 {
		return
	}
	if !buckets.coordinatorActive {
		if subject.Kind == "account" {
			state.accounts++
		} else {
			state.parents++
		}
		if state.accounts > 256 || state.parents > 64 {
			state.invalid = true
			return
		}
		buckets.coordinatorActive = true
		state.active = append(state.active, coordinatorActivity{subject, buckets})
	}
	counters := &directionalBucket(buckets, upload).coordinator
	target := &counters.attempted
	if admitted {
		target = &counters.admitted
	}
	if uint64(size) > coordinatorMaxInteger-*target {
		state.invalid = true
		return
	}
	*target += uint64(size)
}

func coordinatorBucketMode(bucket *tailnetRateBucket) CoordinatorMode {
	switch bucket.mode {
	case tailnetLimited:
		return CoordinatorMode{Mode: "limited", Rate: bucket.rate}
	case tailnetUnlimited:
		return CoordinatorMode{Mode: "unlimited"}
	default:
		return CoordinatorMode{Mode: "deny"}
	}
}
func coordinatorBucketDirection(bucket *tailnetRateBucket) CoordinatorDirection {
	return CoordinatorDirection{Attempted: bucket.coordinator.attempted, Admitted: bucket.coordinator.admitted, Effective: coordinatorBucketMode(bucket)}
}

func (r *tailnetRateRegistry) coordinatorSnapshot() ([]CoordinatorEntry, int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	state := r.coordinator
	if state == nil {
		return nil, 0, errors.New("coordinator disabled")
	}
	now := r.now()
	elapsed := now.Sub(state.started)
	valid := !state.invalid && elapsed >= time.Millisecond && elapsed <= 120*time.Second
	entries := make([]CoordinatorEntry, 0, len(state.active))
	for _, activity := range state.active {
		buckets := activity.buckets
		if valid {
			entries = append(entries, CoordinatorEntry{activity.subject, coordinatorBucketDirection(&buckets.upload), coordinatorBucketDirection(&buckets.download)})
		}
		buckets.upload.coordinator, buckets.download.coordinator = coordinatorCounters{}, coordinatorCounters{}
		buckets.coordinatorActive = false
	}
	state.active = state.active[:0]
	state.accounts, state.parents, state.invalid, state.started = 0, 0, false, now
	if !valid {
		return nil, 0, errors.New("invalid coordinator window or capacity")
	}
	return entries, elapsed.Milliseconds(), nil
}

func coordinatorIntersection(rate int, applicable bool, overlay CoordinatorMode) (int, bool) {
	if !applicable || overlay.Mode == "deny" {
		return 0, false
	}
	if overlay.Mode == "limited" && (rate == 0 || overlay.Rate < rate) {
		rate = overlay.Rate
	}
	return rate, true
}

func (r *tailnetRateRegistry) updateCoordinatorBuckets(subject CoordinatorSubject, buckets *tailnetRateBuckets, policy tailnetRatePolicy, applicable, first bool, now time.Time) {
	// Compose first, then update once. An intermediate static unlimited/limited
	// transition must not alter finiteHistory or balances of an unchanged result.
	up, down, upAllowed, downAllowed := policy.upload, policy.download, applicable, applicable
	if r.coordinator != nil {
		if overlay, ok := r.coordinator.overlay[subject]; ok {
			up, upAllowed = coordinatorIntersection(up, applicable, overlay.upload)
			down, downAllowed = coordinatorIntersection(down, applicable, overlay.download)
		}
	}
	// No static accounts leaf is represented by an empty unlimited policy.
	// Its dynamic finite overlay still needs capacity for a legal full packet.
	burst := policy.burst
	if burst == 0 {
		burst = derp.MaxPacketSize
	}
	buckets.upload.update(up, burst, upAllowed, first, now)
	buckets.download.update(down, burst, downAllowed, first, now)
}

func (r *tailnetRateRegistry) coordinatorBuckets(subject CoordinatorSubject) *tailnetRateBuckets {
	if subject.Kind == "network" {
		return r.buckets[subject.Network]
	}
	return r.accounts[tailnetAccountIdentity{subject.Network, "user", subject.Account}]
}

func (r *tailnetRateRegistry) applyCoordinator(response coordinatorResponse, node, boot string, seq uint64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.coordinator == nil || response.node != node || response.boot != boot || response.seq != seq || response.generation < r.coordinator.generation {
		return errors.New("coordinator response binding or generation mismatch")
	}
	for _, allocation := range response.allocations {
		if r.coordinatorBuckets(allocation.CoordinatorSubject) == nil {
			return errors.New("unregistered coordinator identity")
		}
	}
	now := r.now()
	for _, allocation := range response.allocations {
		subject := allocation.CoordinatorSubject
		if previous, ok := r.coordinator.overlay[subject]; ok && previous.upload == allocation.upload && previous.download == allocation.download {
			continue
		}
		r.coordinator.overlay[subject] = allocation
		policy, applicable := r.config.policyFor(subject.Network)
		if subject.Kind == "account" {
			policy, applicable = r.config.accountPolicyFor(tailnetAccountIdentity{subject.Network, "user", subject.Account})
		}
		r.updateCoordinatorBuckets(subject, r.coordinatorBuckets(subject), policy, applicable, false, now)
	}
	r.coordinator.generation = response.generation
	return nil
}

func (s *Server) coordinatorRegistry() (*tailnetRateRegistry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tailnetRate == nil || !s.tailnetAccountIdentity {
		return nil, errors.New("coordinator requires tailnet account identity")
	}
	return s.tailnetRate, nil
}

// EnableTailnetCoordinator enables bounded window counters before starting the
// reporter. It never changes identity capability or the static policy.
func (s *Server) EnableTailnetCoordinator() error {
	r, err := s.coordinatorRegistry()
	if err != nil {
		return err
	}
	r.enableCoordinator()
	return nil
}

// TailnetCoordinatorSnapshot exchanges a complete window, even if its caller
// later fails to send it. Invalid windows are discarded, never truncated.
func (s *Server) TailnetCoordinatorSnapshot() ([]CoordinatorEntry, int64, error) {
	r, err := s.coordinatorRegistry()
	if err != nil {
		return nil, 0, err
	}
	return r.coordinatorSnapshot()
}

// ApplyTailnetCoordinatorResponse validates the whole wire document before any
// update. Missing allocations retain their prior overlay for this process.
func (s *Server) ApplyTailnetCoordinatorResponse(raw []byte, node, boot string, seq uint64) (uint64, error) {
	response, err := parseCoordinatorResponse(raw)
	if err != nil {
		return 0, errors.New("invalid coordinator response")
	}
	r, err := s.coordinatorRegistry()
	if err != nil {
		return 0, err
	}
	if err := r.applyCoordinator(response, node, boot, seq); err != nil {
		return 0, err
	}
	return response.revision, nil
}
