// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

package derpserver

import (
	"sync"
	"time"
)

type tailnetBucketMode uint8

const (
	tailnetDeny tailnetBucketMode = iota
	tailnetUnlimited
	tailnetLimited
)

type tailnetRateBucket struct {
	mode          tailnetBucketMode
	rate, burst   int
	tokens        float64
	last          time.Time
	finiteHistory bool
}

// settle first compares time-to-fill, avoiding an overflowing duration product.
func (b *tailnetRateBucket) settle(now time.Time) {
	if b.mode != tailnetLimited || !now.After(b.last) {
		return
	}
	elapsed := now.Sub(b.last).Seconds()
	missing := float64(b.burst) - b.tokens
	if elapsed >= missing/float64(b.rate) {
		b.tokens = float64(b.burst)
	} else {
		b.tokens += elapsed * float64(b.rate)
	}
	b.last = now
}

func (b *tailnetRateBucket) update(rate, burst int, applicable, first bool, now time.Time) {
	b.settle(now)
	mode := tailnetDeny
	if applicable {
		mode = tailnetUnlimited
		if rate > 0 {
			mode = tailnetLimited
		}
	}
	if mode == tailnetLimited {
		if !b.finiteHistory {
			b.tokens = 0
			if first {
				b.tokens = float64(burst)
			}
			b.finiteHistory = true
		}
		if b.tokens > float64(burst) {
			b.tokens = float64(burst)
		}
	}
	b.mode, b.rate, b.burst, b.last = mode, rate, burst, now
}

func (b *tailnetRateBucket) allow(size int, now time.Time) bool {
	if size < 0 || b.mode == tailnetDeny {
		return false
	}
	if b.mode == tailnetUnlimited {
		return true
	}
	b.settle(now)
	if float64(size) > b.tokens {
		return false
	}
	b.tokens -= float64(size)
	return true
}

type tailnetRateBuckets struct{ upload, download tailnetRateBucket }

func (b *tailnetRateBuckets) update(policy tailnetRatePolicy, applicable, first bool, now time.Time) {
	b.upload.update(policy.upload, policy.burst, applicable, first, now)
	b.download.update(policy.download, policy.burst, applicable, first, now)
}

// Subject history is deliberately retained for this process's lifetime, even
// when admission is denied or unlimited. Reconnects must not mint fresh bursts.
// The mutex linearizes admission, both directions, and complete reloads; no I/O
// or callbacks other than the local clock occur while it is held.
type tailnetRateRegistry struct {
	mu      sync.Mutex
	config  tailnetRateConfig
	buckets map[string]*tailnetRateBuckets
	now     func() time.Time
}

func newTailnetRateRegistry(cfg tailnetRateConfig, now func() time.Time) *tailnetRateRegistry {
	return &tailnetRateRegistry{config: cfg, buckets: make(map[string]*tailnetRateBuckets), now: now}
}

func (r *tailnetRateRegistry) admit(subject string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !validRateLimitSubject(subject) {
		return false
	}
	policy, applicable := r.config.policyFor(subject)
	if r.buckets[subject] == nil {
		buckets := new(tailnetRateBuckets)
		buckets.update(policy, applicable, true, r.now())
		r.buckets[subject] = buckets
	}
	return applicable
}

func (r *tailnetRateRegistry) apply(cfg tailnetRateConfig) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	for subject, buckets := range r.buckets {
		policy, applicable := cfg.policyFor(subject)
		buckets.update(policy, applicable, false, now)
	}
	r.config = cfg
}

func (r *tailnetRateRegistry) allow(subject string, size int, upload bool) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	buckets := r.buckets[subject]
	if buckets == nil {
		return false
	} // only trusted admission may register identity
	if upload {
		return buckets.upload.allow(size, r.now())
	}
	return buckets.download.allow(size, r.now())
}
