// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

package derpserver

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const accountBenchPacket = 1200

// A fixed clock excludes time acquisition/refill cost. The finite burst is a
// valid configured value, not an injected token balance. Use fixed iteration
// runs (e.g. -benchtime=200000x): every packet must succeed; exhaustion fails.
func accountBenchFixture(tb testing.TB, count int, children bool) (*tailnetRateRegistry, []tailnetAccountIdentity) {
	tb.Helper()
	leaf := tailnetRatePolicy{upload: maxTailnetRateValue, download: maxTailnetRateValue, burst: maxTailnetRateValue}
	parent := leaf
	if children {
		parent.accounts = &tailnetAccountPolicies{defaultPolicy: leaf, overrides: make(map[string]tailnetRatePolicy)}
	}
	cfg := tailnetRateConfig{policies: map[string]tailnetRatePolicy{"network-a": parent}}
	now := time.Unix(1000, 0)
	registry := newTailnetRateRegistry(cfg, func() time.Time { return now })
	ids := make([]tailnetAccountIdentity, count)
	for i := range ids {
		ids[i] = tailnetAccountIdentity{network: "network-a", kind: "user", key: fmt.Sprintf("user%d@synthetic.test", i)}
		if !registry.admitAccount(ids[i]) {
			tb.Fatal("fixture admission denied")
		}
	}
	return registry, ids
}

// Historical no-scan experiment, retained as a benchmark-only control. The
// production allowAccount now also omits the repeated identity scan; v2 hot
// benchmarks below still call production, never this helper. Eligibility relies
// on immutable, previously validated identities and exact map registration.
func accountBenchNoScan(r *tailnetRateRegistry, id tailnetAccountIdentity, size int, upload bool) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if size < 0 {
		return false
	}
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

func TestAccountBenchmarkModel(t *testing.T) {
	for _, children := range []bool{false, true} {
		real, ids := accountBenchFixture(t, 2, children)
		model, _ := accountBenchFixture(t, 2, children)
		missing := tailnetAccountIdentity{network: "network-a", kind: "user", key: "missing@synthetic.test"}
		for _, id := range []tailnetAccountIdentity{ids[0], ids[1], missing, {}} {
			for _, size := range []int{1200, -1, maxTailnetRateValue, 0} {
				for _, upload := range []bool{true, false} {
					got, want := accountBenchNoScan(model, id, size, upload), real.allowAccount(id, size, upload)
					if got != want {
						t.Fatalf("model mismatch: children=%v size=%d", children, size)
					}
				}
			}
		}
		if !reflect.DeepEqual(real.buckets, model.buckets) || !reflect.DeepEqual(real.accounts, model.accounts) {
			t.Fatal("model balances differ from production")
		}
	}
}

func BenchmarkAccountHot(b *testing.B) {
	for _, count := range []int{1, 1000} {
		for _, mode := range []string{"v1", "v2-off", "v2-dual", "model-off", "model-dual"} {
			b.Run(fmt.Sprintf("%s/accounts=%d", mode, count), func(b *testing.B) {
				accountBenchHot(b, count, mode)
			})
		}
	}
}

func accountBenchHot(b *testing.B, count int, mode string) {
	children := mode == "v2-dual" || mode == "model-dual"
	registry, ids := accountBenchFixture(b, count, children)
	if mode == "v1" {
		// v1 registers only the parent; accounts count denotes logical clients.
		registry.accounts = make(map[tailnetAccountIdentity]*tailnetRateBuckets)
	}
	allow := registry.allowAccount
	if mode == "v1" {
		allow = func(id tailnetAccountIdentity, size int, upload bool) bool {
			return registry.allow(id.network, size, upload)
		}
	} else if mode == "model-off" || mode == "model-dual" {
		allow = func(id tailnetAccountIdentity, size int, upload bool) bool {
			return accountBenchNoScan(registry, id, size, upload)
		}
	}
	var failures, workerID atomic.Uint64
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		index := int(workerID.Add(1)-1) % len(ids)
		upload := true
		for pb.Next() {
			if !allow(ids[index], accountBenchPacket, upload) {
				failures.Add(1)
			}
			index++
			if index == len(ids) {
				index = 0
			}
			upload = !upload
		}
	})
	b.StopTimer()
	if failed := failures.Load(); failed != 0 {
		b.Fatalf("%d rejected packets; reduce fixed iteration count", failed)
	}
}

func accountBenchConfigFile(tb testing.TB, registry *tailnetRateRegistry) string {
	tb.Helper()
	path := filepath.Join(tb.TempDir(), "rates.json")
	leaf := `{"upload_bytes_per_second":2147483647,"download_bytes_per_second":2147483647,"burst_bytes":2147483647}`
	data := `{"tailnets":{"network-a":{"upload_bytes_per_second":2147483647,"download_bytes_per_second":2147483647,"burst_bytes":2147483647,"accounts":{"default":` + leaf + `}}}}`
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		tb.Fatal(err)
	}
	parsed, err := parseTailnetRateConfig([]byte(data))
	if err != nil || !reflect.DeepEqual(parsed, registry.config) {
		tb.Fatalf("reload fixture mismatch: %v", err)
	}
	return path
}

func BenchmarkAccountReload(b *testing.B) {
	for _, count := range []int{1000, 100000} {
		for _, mode := range []string{"apply", "load"} {
			b.Run(fmt.Sprintf("%s/accounts=%d", mode, count), func(b *testing.B) {
				registry, _ := accountBenchFixture(b, count, true)
				server := &Server{tailnetRate: registry, tailnetAccountIdentity: true}
				path := accountBenchConfigFile(b, registry)
				cfg := registry.config
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					if mode == "apply" {
						registry.apply(cfg)
					} else if err := server.LoadAndApplyTailnetRateConfig(path); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}

// Release the real apply at its clock callback (inside both locks for Load).
// Packet and Server.mu probes are already queued before release. Report only
// residual latency AFTER release, excluding the artificial rendezvous hold.
// A Server.mu-only probe isolates additional lock scope; it is not an e2e packet.
func accountBenchBlocked(tb testing.TB, count int, load bool) (time.Duration, time.Duration, time.Duration) {
	tb.Helper()
	registry, ids := accountBenchFixture(tb, count, true)
	server := &Server{tailnetRate: registry, tailnetAccountIdentity: true}
	path := accountBenchConfigFile(tb, registry)
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	var once sync.Once
	registry.now = func() time.Time {
		once.Do(func() { close(entered); <-release })
		return time.Unix(1000, 0)
	}
	go func() {
		if load {
			done <- server.LoadAndApplyTailnetRateConfig(path)
		} else {
			registry.apply(registry.config)
			done <- nil
		}
	}()
	<-entered
	packetReady, serverReady := make(chan struct{}), make(chan struct{})
	packetDone, serverDone := make(chan time.Time, 1), make(chan time.Time, 1)
	go func() {
		close(packetReady)
		if !registry.allowAccount(ids[0], accountBenchPacket, true) {
			panic("blocked packet rejected")
		}
		packetDone <- time.Now()
	}()
	go func() {
		close(serverReady)
		server.mu.Lock()
		server.mu.Unlock()
		serverDone <- time.Now()
	}()
	<-packetReady
	<-serverReady
	// Yield so ready goroutines can reach their locks; not included in metrics.
	runtime.Gosched()
	releasedAt := time.Now()
	close(release)
	if err := <-done; err != nil {
		tb.Fatal(err)
	}
	reloadTime := time.Since(releasedAt)
	packetTime := (<-packetDone).Sub(releasedAt)
	serverTime := (<-serverDone).Sub(releasedAt)
	if serverTime < 0 {
		serverTime = 0
	} // direct apply never takes Server.mu
	return reloadTime, packetTime, serverTime
}

func BenchmarkAccountReloadBlocking(b *testing.B) {
	for _, count := range []int{1000, 100000} {
		for _, load := range []bool{false, true} {
			b.Run(fmt.Sprintf("load=%v/accounts=%d", load, count), func(b *testing.B) {
				var reloadTotal, packetTotal, serverTotal, packetMax, serverMax time.Duration
				for i := 0; i < b.N; i++ {
					reload, packet, server := accountBenchBlocked(b, count, load)
					reloadTotal += reload
					packetTotal += packet
					serverTotal += server
					if packet > packetMax {
						packetMax = packet
					}
					if server > serverMax {
						serverMax = server
					}
				}
				b.ReportMetric(float64(reloadTotal.Nanoseconds())/float64(b.N), "resume-apply-ns/op")
				b.ReportMetric(float64(packetTotal.Nanoseconds())/float64(b.N), "packet-wait-ns/op")
				b.ReportMetric(float64(serverTotal.Nanoseconds())/float64(b.N), "server-wait-ns/op")
				b.ReportMetric(float64(packetMax.Nanoseconds()), "packet-max-ns")
				b.ReportMetric(float64(serverMax.Nanoseconds()), "server-max-ns")
			})
		}
	}
}
