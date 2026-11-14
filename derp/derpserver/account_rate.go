// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

package derpserver

import "time"

func (r *tailnetRateRegistry) admitAccount(id tailnetAccountIdentity) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !id.valid() {
		return false
	}
	now := r.now()
	parent, applicable := r.config.policyFor(id.network)
	if r.buckets[id.network] == nil {
		buckets := new(tailnetRateBuckets)
		buckets.update(parent, applicable, true, now)
		r.buckets[id.network] = buckets
	}
	if r.accounts[id] == nil {
		policy, applicable := r.config.accountPolicyFor(id)
		buckets := new(tailnetRateBuckets)
		buckets.update(policy, applicable, true, now)
		r.accounts[id] = buckets
	}
	return applicable
}

func directionalBucket(b *tailnetRateBuckets, upload bool) *tailnetRateBucket {
	if upload {
		return &b.upload
	}
	return &b.download
}

func (b *tailnetRateBucket) canSpend(size int, now time.Time) bool {
	b.settle(now)
	return b.mode == tailnetUnlimited || (b.mode == tailnetLimited && float64(size) <= b.tokens)
}

// Every layer settles at the same instant, then every check succeeds before
// either balance changes. Packet paths may not create identities or buckets.
func (r *tailnetRateRegistry) allowAccount(id tailnetAccountIdentity, size int, upload bool) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if size < 0 {
		return false
	}
	// admitAccount validates immutable identities before registration. Exact
	// map membership is the packet-path boundary; do not rescan opaque keys.
	parent, child := r.buckets[id.network], r.accounts[id]
	if parent == nil || child == nil {
		return false
	}
	now := r.now()
	pb, cb := directionalBucket(parent, upload), directionalBucket(child, upload)
	parentOK, childOK := pb.canSpend(size, now), cb.canSpend(size, now)
	if !parentOK || !childOK {
		return false
	}
	if pb.mode == tailnetLimited {
		pb.tokens -= float64(size)
	}
	if cb.mode == tailnetLimited {
		cb.tokens -= float64(size)
	}
	return true
}
