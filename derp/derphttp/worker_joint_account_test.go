// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

package derphttp_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"tailscale.com/derp"
	"tailscale.com/derp/derpserver"
	"tailscale.com/types/key"
)

func jointAccountConfig(parentUp, parentDown, childUp, childDown int, accounts bool) string {
	leaf := fmt.Sprintf(`{"upload_bytes_per_second":%d,"download_bytes_per_second":%d}`, childUp, childDown)
	children := ""
	if accounts {
		children = `,"accounts":{"default":` + leaf + `,"tagged":` + leaf + `}`
	}
	return fmt.Sprintf(`{"tailnets":{"network0":{"upload_bytes_per_second":%d,"download_bytes_per_second":%d%s},"network1":{"upload_bytes_per_second":0,"download_bytes_per_second":0%s},"free":{"upload_bytes_per_second":0,"download_bytes_per_second":0}}}`, parentUp, parentDown, children, children)
}

func (h *jointHarness) accountServer(t *testing.T, callback, config string) string {
	t.Helper()
	if callback == "" {
		t.Fatal("v2 Worker callback missing: runner has not wired account scenarios")
	}
	s := derpserver.New(key.NewNode(), t.Logf)
	t.Cleanup(func() { s.Close() })
	s.SetVerifyClientURL(callback)
	s.SetVerifyClientURLFailOpen(false)
	if err := s.SetTailnetAccountIdentity(true); err != nil {
		t.Fatal(err)
	}
	filename := filepath.Join(h.run, fmt.Sprintf("account-policy-%d.json", time.Now().UnixNano()))
	jointWrite(t, filename, config)
	if err := s.LoadAndApplyTailnetRateConfig(filename); err != nil {
		t.Fatal(err)
	}
	endpoint := httptest.NewServer(derpserver.Handler(s))
	t.Cleanup(endpoint.Close)
	return endpoint.URL
}

func (h *jointHarness) accountScenarios(t *testing.T) {
	for _, direction := range []string{"upload", "download"} {
		for _, layer := range []string{"parent", "child", "both", "unlimited", "override_zero"} {
			t.Run("v2_"+direction+"_"+layer, func(t *testing.T) { h.accountPolicing(t, direction, layer) })
		}
	}
	noAccounts := jointAccountConfig(0, 0, 0, 0, false)
	t.Run("v2_handshake_without_accounts", func(t *testing.T) { h.client(t, h.accountServer(t, h.ready.AccountNormal, noAccounts), "same0", true) })
	t.Run("v2_unknown_without_accounts_rejected", func(t *testing.T) {
		url := h.accountServer(t, h.ready.AccountNormal, noAccounts)
		h.client(t, url, "unknownaccount", false)
		h.client(t, h.accountServer(t, h.ready.AccountUnknown, noAccounts), "same0", false)
	})
	t.Run("v2_conflicting_account_rejected", func(t *testing.T) {
		h.client(t, h.accountServer(t, h.ready.AccountConflict, noAccounts), "same0", false)
	})
	t.Run("v2_new_worker_missing_secret_rejected", func(t *testing.T) { h.client(t, h.accountServer(t, h.ready.Normal, noAccounts), "same0", false) })
	t.Run("v2_old_worker_downgrade_rejected", func(t *testing.T) { h.client(t, h.accountServer(t, h.ready.OldWorker, noAccounts), "same0", false) })
	t.Run("v1_old_worker_extended_snapshot_compatible", func(t *testing.T) {
		h.client(t, h.server(t, h.ready.OldWorker, jointConfig(0, 0), true), "unknownaccount", true)
	})
	t.Run("v2_allow_only_rejected", func(t *testing.T) { h.client(t, h.accountServer(t, h.ready.Legacy, noAccounts), "same0", false) })
	t.Run("v1_extended_snapshot_compatible", func(t *testing.T) {
		h.client(t, h.server(t, h.ready.AccountNormal, jointConfig(0, 0), true), "unknownaccount", true)
	})
	t.Run("v2_binary_sighup_existing_connections", h.accountSighup)
	t.Run("v1_binary_initial_accounts_rejected", h.accountStartupRejected)
}

func (h *jointHarness) accountStartupRejected(t *testing.T) {
	policy := filepath.Join(h.run, "startup-accounts.json")
	jointWrite(t, policy, jointAccountConfig(0, 0, 1, 0, true))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, h.ready.Binary, "-a=127.0.0.1:0", "-http-port=-1", "-stun=false", "-mesh-psk-file=", "-mesh-with=", "-bootstrap-dns-names=", "-unpublished-bootstrap-dns-names=", "-c", filepath.Join(h.run, "startup-private.json"), "-verify-clients=false", "-verify-client-url-fail-open=false", "-verify-client-url", h.ready.Normal, "-tailnet-rate-config", policy)
	output, err := cmd.CombinedOutput()
	jointWrite(t, filepath.Join(h.run, "startup-rejected.log"), string(output))
	if ctx.Err() != nil || err == nil || !strings.Contains(string(output), "accounts require startup tailnet account identity capability") {
		t.Fatalf("expected capability rejection, got err=%v context=%v output=%s", err, ctx.Err(), output)
	}
}

// Read the actual Worker response rather than reproducing its HMAC in Go.
func (h *jointHarness) accountZeroOverride(t *testing.T, config string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(h.run, "wire-accountNormal.json"))
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Body struct {
			Identity struct{ Account struct{ Kind, Key string } }
		}
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		t.Fatal(err)
	}
	account := wire.Body.Identity.Account
	if account.Kind != "user" || !strings.HasPrefix(account.Key, "h1_") || len(account.Key) != 67 {
		t.Fatalf("Worker returned invalid registrant identity: %+v", account)
	}
	return strings.ReplaceAll(config, `"accounts":{"default":`, fmt.Sprintf(`"accounts":{"overrides":{%q:{"upload_bytes_per_second":0,"download_bytes_per_second":0}},"default":`, account.Key))
}

func (h *jointHarness) accountPolicing(t *testing.T, direction, layer string) {
	parent, child := 0, 0
	if layer == "parent" || layer == "both" {
		parent = 1
	}
	if layer == "child" || layer == "both" || layer == "override_zero" {
		child = 1
	}
	pu, pd, cu, cd := parent, 0, child, 0
	if direction == "download" {
		pu, pd, cu, cd = 0, parent, 0, child
	}
	config := jointAccountConfig(pu, pd, cu, cd, true)
	if layer == "override_zero" {
		config = h.accountZeroOverride(t, config)
	}
	url := h.accountServer(t, h.ready.AccountNormal, config)
	clients := map[string]*jointClient{}
	for _, name := range []string{"same0", "same1", "different", "tagged0", "tagged1", "cross", "free"} {
		clients[name] = h.client(t, url, name, true)
	}
	packet := byte(0)
	transfer := func(name string, allow bool) {
		t.Helper()
		packet++
		sender, destination, receiver, source := clients[name], "free", clients["free"], name
		if direction == "download" {
			sender, destination, receiver, source = clients["free"], name, clients[name], "free"
		}
		h.send(t, sender, destination, packet, derp.MaxPacketSize)
		if allow {
			h.receive(t, receiver, source, packet, derp.MaxPacketSize)
		} else {
			jointAbsent(t, receiver)
		}
	}
	unlimitedAccount := layer == "unlimited" || layer == "override_zero"
	transfer("same0", true)
	transfer("same1", unlimitedAccount)       // Two untagged keys, one registrant bucket.
	transfer("different", parent == 0)        // Different registrant, same parent.
	transfer("tagged0", unlimitedAccount)     // Same registrant as same0/same1, including its zero override.
	transfer("tagged1", layer == "unlimited") // Same registrant as different, not a global tagged bucket.
	transfer("cross", true)                   // Same synthetic user, different network.
	if layer == "override_zero" {
		transfer("tagged0", true) // Repeated full packets prove zero is unlimited, not a fresh burst.
		transfer("same1", true)
	}
	// Exhaustion in the tested direction must not limit the opposite direction.
	if direction == "upload" {
		h.send(t, clients["free"], "same1", 90, derp.MaxPacketSize)
		h.receive(t, clients["same1"], "free", 90, derp.MaxPacketSize)
	} else {
		h.send(t, clients["same1"], "free", 90, derp.MaxPacketSize)
		h.receive(t, clients["free"], "same1", 90, derp.MaxPacketSize)
	}
	h.send(t, clients["free"], "free", 99, derp.MaxPacketSize)
	h.receive(t, clients["free"], "free", 99, derp.MaxPacketSize)
}

func (h *jointHarness) accountSighup(t *testing.T) {
	if h.ready.AccountPID == 0 {
		t.Fatal("account-capable binary missing")
	}
	process, err := os.FindProcess(h.ready.AccountPID)
	if err != nil {
		t.Fatal(err)
	}
	defer process.Release()
	jointWait(t, func() bool { // Wait for the real listener, never launch with --dev.
		return jointHTTPReady(h.ready.AccountBinaryURL)
	})
	first := h.client(t, h.ready.AccountBinaryURL, "same0", true)
	second := h.client(t, h.ready.AccountBinaryURL, "same1", true)
	free := h.client(t, h.ready.AccountBinaryURL, "free", true)
	packet := byte(20)
	transfer := func(sender *jointClient, name string, allow bool) {
		t.Helper()
		packet++
		h.send(t, sender, "free", packet, derp.MaxPacketSize)
		if allow {
			h.receive(t, free, name, packet, derp.MaxPacketSize)
		} else {
			jointAbsent(t, free)
		}
		packet++
		h.send(t, free, "free", packet, derp.MaxPacketSize)
		h.receive(t, free, "free", packet, derp.MaxPacketSize)
	}
	addresses := map[*jointClient]string{}
	for _, client := range []*jointClient{first, second, free} {
		address, err := client.client.LocalAddr()
		if err != nil {
			t.Fatal(err)
		}
		addresses[client] = address.String()
	}
	reload := func(config string, valid bool) {
		t.Helper()
		ack := "tailnet rate config reloaded successfully"
		if !valid {
			ack = "tailnet rate config reload failed"
		}
		h.reloadAt(t, process, h.ready.AccountPolicy, h.ready.AccountLog, config, ack)
		for client, original := range addresses {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			err := client.client.Ping(ctx)
			cancel()
			if err != nil {
				t.Fatalf("existing connection ping failed: %v", err)
			}
			address, err := client.client.LocalAddr()
			if err != nil || address.String() != original {
				t.Fatalf("connection replaced: %s -> %v (%v)", original, address, err)
			}
		}
	}
	transfer(first, "same0", true)
	transfer(second, "same1", true)
	reload(jointAccountConfig(0, 0, 2, 0, true), true) // Add accounts to already-admitted identities: no fresh burst.
	transfer(first, "same0", false)
	reload(jointAccountConfig(0, 0, 1048576, 0, true), true)
	if delivered := h.accountPulse(t, first, free, "same0"); delivered < 2 {
		t.Fatalf("high-rate positive control delivered %d packets, want at least 2", delivered)
	}
	reload(jointAccountConfig(0, 0, 1, 0, true), true) // Tighten; preserve at most one existing burst.
	if delivered := h.accountPulse(t, second, free, "same1"); delivered > 1 {
		t.Fatalf("tightened account delivered %d full packets, want at most inherited burst of 1", delivered)
	}
	transfer(second, "same1", false)
	reload(jointAccountConfig(0, 0, 0, 0, false), true) // Disable child policy without replacing connections.
	transfer(first, "same0", true)
	transfer(second, "same1", true)
	reload(jointAccountConfig(0, 0, 1, 0, true), true) // Restore frozen child history.
	transfer(second, "same1", false)
	reload(`{"tailnets":{"network0":{"upload_bytes_per_second":0,"download_bytes_per_second":0,"accounts":{}}}}`, false)
	transfer(first, "same0", false) // Invalid reload preserves the complete prior policy.
	reload(`{"tailnets":{"free":{"upload_bytes_per_second":0,"download_bytes_per_second":0}}}`, true)
	transfer(first, "same0", false) // Removed parent is deny, not default/unlimited.
	reload(jointAccountConfig(0, 0, 0, 0, false), true)
	transfer(first, "same0", true)
	transfer(second, "same1", true)
	t.Logf("same two held-key connections survived account add/tighten/disable/restore, invalid reload and parent delete/restore; PID=%d", process.Pid)
}

// A bounded packet train distinguishes a tightened limiter from an ignored
// reload without assuming an exact token balance. Stop sending halfway through
// the observation window so in-flight packets cannot bleed into the next phase.
func (h *jointHarness) accountPulse(t *testing.T, sender, receiver *jointClient, source string) int {
	t.Helper()
	tick := time.NewTicker(25 * time.Millisecond)
	defer tick.Stop()
	now := time.Now()
	sent, delivered, err := jointAccountPulse(sender, receiver.packets, h.keys["free"].Public(), h.keys[source].Public(), tick.C, now.Add(250*time.Millisecond), now.Add(500*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("reload probe: %d numbered packets sent, %d delivered", sent, delivered)
	return delivered
}

func jointAccountPulse(sender *jointClient, packets <-chan jointPacket, destination, source key.NodePublic, ticks <-chan time.Time, stopAt, finishAt time.Time) (sent, delivered int, err error) {
	stopSending := time.NewTimer(time.Until(stopAt))
	defer stopSending.Stop()
	deadline := time.NewTimer(time.Until(finishAt))
	defer deadline.Stop()
	// Eight full packets cap this probe at 512 KiB of payload, regardless of
	// queued ticks or scheduling delays. No asynchronous Send can outlive it.
	const packetBudget = 8
	writeDeadline := stopAt
	if finishAt.Before(writeDeadline) {
		writeDeadline = finishAt
	}
	if err := sender.conn.SetWriteDeadline(writeDeadline); err != nil {
		return 0, 0, err
	}
	defer func() { err = errors.Join(err, sender.conn.SetWriteDeadline(time.Time{})) }()
	for {
		if !time.Now().Before(finishAt) {
			return sent, delivered, nil
		}
		if !time.Now().Before(stopAt) || sent == packetBudget {
			ticks = nil
		}
		select {
		case <-ticks:
			// A ready stop timer and a queued tick can win select in either order.
			if !time.Now().Before(writeDeadline) {
				ticks = nil
				continue
			}
			sent++
			if err := sender.client.Send(destination, bytes.Repeat([]byte{128 + byte(sent)}, derp.MaxPacketSize)); err != nil {
				return sent, delivered, err
			}
			// derphttp.Send may consume a transport error while scheduling a
			// reconnect. Expiry is still a failed probe, never a policing drop.
			if !time.Now().Before(writeDeadline) {
				return sent, delivered, context.DeadlineExceeded
			}
		case packet := <-packets:
			if packet.err != nil {
				return sent, delivered, packet.err
			}
			if packet.source != source || len(packet.data) != derp.MaxPacketSize || packet.data[0] <= 128 || int(packet.data[0]) > 128+sent {
				return sent, delivered, fmt.Errorf("unexpected packet in reload probe train")
			}
			for _, value := range packet.data {
				if value != packet.data[0] {
					return sent, delivered, fmt.Errorf("corrupted numbered probe payload")
				}
			}
			delivered++
		case <-stopSending.C:
			ticks = nil // Stopping the ticker alone does not discard a queued tick.
		case <-deadline.C:
			return sent, delivered, nil
		}
	}
}
