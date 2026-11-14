// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

package derphttp_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"tailscale.com/derp/derpserver"
)

type jointCoordinatorBinary struct {
	URL, Policy, Log string
	PID              int
}
type jointCoordinatorWire struct {
	Report   derpserver.CoordinatorReport `json:"report"`
	Status   int                          `json:"status"`
	Response struct {
		Allocations []struct {
			derpserver.CoordinatorSubject
			Upload, Download derpserver.CoordinatorMode
		}
	} `json:"response"`
}
type jointCoordinatorClients struct{ up, down, free *jointClient }

func (h *jointHarness) coordinatorWire(t *testing.T, node int) jointCoordinatorWire {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(h.run, fmt.Sprintf("coordinator-joint%d.json", node)))
	if os.IsNotExist(err) {
		return jointCoordinatorWire{}
	}
	if err != nil {
		t.Fatal(err)
	}
	var wire jointCoordinatorWire
	if err := json.Unmarshal(data, &wire); err != nil {
		t.Fatal(err)
	}
	return wire
}

func jointAllocation(wire jointCoordinatorWire, kind, account string) (up, down int) {
	for _, allocation := range wire.Response.Allocations {
		if allocation.Kind == kind && allocation.Network == "network0" && allocation.Account == account {
			return allocation.Upload.Rate, allocation.Download.Rate
		}
	}
	return 0, 0
}

func (h *jointHarness) coordinatorPulse(t *testing.T, clients jointCoordinatorClients) (int, int) {
	t.Helper()
	up := h.accountPulse(t, clients.up, clients.free, "same0")
	tick := time.NewTicker(25 * time.Millisecond)
	defer tick.Stop()
	now := time.Now()
	_, down, err := jointAccountPulse(clients.free, clients.down.packets, h.keys["same1"].Public(), h.keys["free"].Public(), tick.C, now.Add(250*time.Millisecond), now.Add(500*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	return up, down
}

func (h *jointHarness) coordinatorUntil(t *testing.T, clients []jointCoordinatorClients, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(100 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("real 30-second coordinator cycles did not converge")
		}
		for _, node := range clients {
			h.coordinatorPulse(t, node)
		}
	}
}

func (h *jointHarness) coordinatorScenarios(t *testing.T) {
	if len(h.ready.Coordinators) != 2 {
		t.Fatal("two coordinator binaries required")
	}
	clients := make([]jointCoordinatorClients, 2)
	for index, binary := range h.ready.Coordinators {
		jointWait(t, func() bool { return jointHTTPReady(binary.URL) })
		clients[index] = jointCoordinatorClients{h.client(t, binary.URL, "same0", true), h.client(t, binary.URL, "same1", true), h.client(t, binary.URL, "free", true)}
	}
	const account = "alpha@synthetic.test"
	var sharedSeq [2]uint64
	if !t.Run("coordinator_two_nodes_shared_account", func(t *testing.T) {
		h.coordinatorUntil(t, clients, func() bool {
			for index := range clients {
				wire := h.coordinatorWire(t, index)
				up, down := jointAllocation(wire, "account", account)
				if wire.Status != 200 || up != 65536 || down != 65536 {
					return false
				}
				applied := false
				for _, entry := range wire.Report.Entries {
					if entry.Kind == "account" && entry.Account == account {
						applied = entry.Upload.Effective.Rate == 65536 && entry.Download.Effective.Rate == 65536 && entry.Upload.Attempted > entry.Upload.Admitted && entry.Download.Attempted > entry.Download.Admitted
					}
				}
				if !applied {
					return false
				}
				sharedSeq[index] = wire.Report.Seq
			}
			return true
		})
		time.Sleep(time.Second)
		for _, node := range clients {
			up, down := h.coordinatorPulse(t, node)
			if up == 0 || down == 0 || up > 2 || down > 2 {
				t.Fatalf("shared payload train up=%d down=%d; want nonzero and limited", up, down)
			}
		}
		t.Logf("two real binaries, Worker v2 identities, account sums upload=131072 download=131072; applied reports seq=%v", sharedSeq)
	}) {
		return
	}
	if !t.Run("coordinator_idle_node_returns_share", func(t *testing.T) {
		h.coordinatorUntil(t, clients[:1], func() bool {
			idle := h.coordinatorWire(t, 1)
			active := h.coordinatorWire(t, 0)
			up, down := jointAllocation(active, "account", account)
			return idle.Report.Seq > sharedSeq[1] && len(idle.Report.Entries) == 0 && up == 131072 && down == 131072
		})
		t.Log("node1 demand stopped without closing clients; node0 regained full account budget in both directions")
	}) {
		return
	}
	var failureSeq uint64
	if !t.Run("coordinator_failure_keeps_overlay_and_payload", func(t *testing.T) {
		jointWrite(t, filepath.Join(h.run, "coordinator-fail"), "fail")
		h.coordinatorUntil(t, clients[:1], func() bool {
			wire := h.coordinatorWire(t, 0)
			if wire.Status != 503 {
				return false
			}
			for _, entry := range wire.Report.Entries {
				if entry.Kind == "account" && entry.Account == account && entry.Upload.Effective.Rate == 131072 && entry.Download.Effective.Rate == 131072 {
					failureSeq = wire.Report.Seq
					return true
				}
			}
			return false
		})
		time.Sleep(time.Second)
		up, down := h.coordinatorPulse(t, clients[0])
		if up == 0 || down == 0 || up > 3 || down > 3 {
			t.Fatalf("outage payload up=%d down=%d", up, down)
		}
		t.Logf("HTTP 503 seq=%d retained effective account overlay, limited nonzero payload continued", failureSeq)
	}) {
		return
	}
	if !t.Run("coordinator_recovery_converges", func(t *testing.T) {
		if err := os.Remove(filepath.Join(h.run, "coordinator-fail")); err != nil {
			t.Fatal(err)
		}
		h.coordinatorUntil(t, clients, func() bool {
			for index := range clients {
				wire := h.coordinatorWire(t, index)
				up, down := jointAllocation(wire, "account", account)
				if wire.Status != 200 || wire.Report.Seq <= failureSeq || up != 65536 || down != 65536 {
					return false
				}
			}
			return true
		})
		t.Log("restored real Worker communication and reconverged to shared budget")
	}) {
		return
	}
	if !t.Run("coordinator_binary_sighup_overlay_intersection", func(t *testing.T) {
		binary := h.ready.Coordinators[0]
		process, err := os.FindProcess(binary.PID)
		if err != nil {
			t.Fatal(err)
		}
		defer process.Release()
		h.reloadAt(t, process, binary.Policy, binary.Log, jointAccountConfig(1, 1, 0, 0, true), "tailnet rate config reloaded successfully")
		up, down := h.coordinatorPulse(t, clients[0])
		if up > 1 || down > 1 {
			t.Fatalf("static tightening bypassed: up=%d down=%d", up, down)
		}
		h.reloadAt(t, process, binary.Policy, binary.Log, jointAccountConfig(0, 0, 0, 0, true), "tailnet rate config reloaded successfully")
		time.Sleep(time.Second)
		up, down = h.coordinatorPulse(t, clients[0])
		if up == 0 || down == 0 || up > 3 || down > 3 {
			t.Fatalf("restored static unlimited lost overlay: up=%d down=%d", up, down)
		}
		for _, node := range clients {
			for _, client := range []*jointClient{node.up, node.down, node.free} {
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				err := client.client.Ping(ctx)
				cancel()
				if err != nil {
					t.Fatal(err)
				}
			}
		}
		t.Log("actual SIGHUP tightened then restored static policy; dynamic overlay retained; all six original no-redial clients ping")
	}) {
		return
	}
	t.Run("coordinator_finite_parent_unlimited_account", func(t *testing.T) {
		different := h.client(t, h.ready.Coordinators[0].URL, "different", true)
		deadline := time.Now().Add(70 * time.Second)
		for {
			h.accountPulse(t, different, clients[0].free, "different")
			wire := h.coordinatorWire(t, 0)
			unlimited := false
			for _, allocation := range wire.Response.Allocations {
				if allocation.Kind == "account" && allocation.Account == "different@synthetic.test" {
					unlimited = allocation.Upload.Mode == "unlimited"
				}
			}
			parentUp, _ := jointAllocation(wire, "network", "")
			if unlimited && parentUp > 0 && parentUp <= 262144 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("unlimited child / finite parent report missing")
			}
		}
		time.Sleep(time.Second)
		delivered := h.accountPulse(t, different, clients[0].free, "different")
		if delivered == 0 || delivered > 3 {
			t.Fatalf("finite parent did not constrain unlimited account: %d", delivered)
		}
		t.Logf("real unlimited-account allocation constrained by finite parent; payload delivered=%d/8", delivered)
	})
}
