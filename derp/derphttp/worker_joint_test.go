// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

package derphttp_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"tailscale.com/derp"
	"tailscale.com/derp/derphttp"
	"tailscale.com/derp/derpserver"
	"tailscale.com/net/netmon"
	"tailscale.com/types/key"
)

type jointReady struct {
	Normal, Unknown, Conflict, Legacy, Binary                 string
	BinaryURL, Policy                                         string
	PID                                                       int
	AccountNormal, AccountUnknown, AccountConflict, OldWorker string
	AccountBinaryURL, AccountPolicy, AccountLog               string
	AccountPID                                                int
}
type jointKey struct {
	Public  string `json:"public"`
	Subject string `json:"subject"`
}
type jointHarness struct {
	run    string
	ready  jointReady
	keys   map[string]key.NodePrivate
	public map[string]jointKey
}

// Opt-in only: Node owns isolated workerd/KV instances; no production credentials.
func TestWorkerJoint(t *testing.T) {
	run := os.Getenv("DERP_JOINT_RUN")
	if run == "" {
		t.Skip("run npm test:integration in derp-admission-worker")
	}
	h := &jointHarness{run: run, keys: map[string]key.NodePrivate{}, public: map[string]jointKey{}}
	for i := 0; i < 10; i++ {
		h.addKey(fmt.Sprintf("node%d", i), fmt.Sprintf("network%d", i))
	}
	h.addKey("other", "network0")
	h.addKey("free", "free")
	for _, name := range []string{"same0", "same1", "different", "tagged0", "tagged1", "unknownaccount"} {
		h.addKey(name, "network0")
	}
	h.addKey("cross", "network1")
	data, err := json.Marshal(h.public)
	if err != nil {
		t.Fatal(err)
	}
	jointWrite(t, filepath.Join(run, "keys.json"), string(data))
	jointWait(t, func() bool {
		data, err := os.ReadFile(filepath.Join(run, "ready.json"))
		if os.IsNotExist(err) {
			return false
		}
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(data, &h.ready); err != nil {
			t.Fatal(err)
		}
		return true
	})
	t.Run("v1_http_key_handshake", func(t *testing.T) { h.client(t, h.server(t, h.ready.Normal, jointConfig(0, 0), true), "node0", true) })
	t.Run("ten_independent_default_subjects", h.tenSubjects)
	for _, direction := range []string{"upload", "download", "both"} {
		t.Run(direction, func(t *testing.T) { h.policing(t, direction) })
	}
	t.Run("legacy_client_new_worker", func(t *testing.T) { h.client(t, h.server(t, h.ready.Normal, "", false), "node0", true) })
	t.Run("legacy_client_legacy_worker", func(t *testing.T) { h.client(t, h.server(t, h.ready.Legacy, "", false), "node0", true) })
	t.Run("new_client_legacy_worker_rejected", func(t *testing.T) { h.client(t, h.server(t, h.ready.Legacy, jointConfig(0, 0), true), "node0", false) })
	t.Run("unknown_conflict_rejected", func(t *testing.T) {
		for _, url := range []string{h.ready.Unknown, h.ready.Conflict} {
			h.client(t, h.server(t, url, jointConfig(0, 0), true), "node0", false)
		}
	})
	t.Run("binary_sighup_existing_connections", h.sighup)
	h.accountScenarios(t)
}

func (h *jointHarness) addKey(name, subject string) {
	private := key.NewNode()
	h.keys[name] = private
	h.public[name] = jointKey{private.Public().String(), subject}
}

func jointWait(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.NewTimer(30 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for !condition() {
		select {
		case <-deadline.C:
			t.Fatal("observable readiness timed out")
		case <-tick.C:
		}
	}
}

func jointConfig(up, down int) string {
	return fmt.Sprintf(`{"default":{"upload_bytes_per_second":%d,"download_bytes_per_second":%d},"tailnets":{"free":{"upload_bytes_per_second":0,"download_bytes_per_second":0}}}`, up, down)
}

func (h *jointHarness) server(t *testing.T, callback, config string, identity bool) string {
	t.Helper()
	s := derpserver.New(key.NewNode(), t.Logf)
	t.Cleanup(func() { s.Close() })
	s.SetVerifyClientURL(callback)
	s.SetVerifyClientURLFailOpen(false)
	if identity {
		filename := filepath.Join(h.run, fmt.Sprintf("policy-%d.json", time.Now().UnixNano()))
		if err := os.WriteFile(filename, []byte(config), 0600); err != nil {
			t.Fatal(err)
		}
		if err := s.LoadAndApplyTailnetRateConfig(filename); err != nil {
			t.Fatal(err)
		}
	}
	endpoint := httptest.NewServer(derpserver.Handler(s))
	t.Cleanup(endpoint.Close)
	return endpoint.URL
}

type jointClient struct {
	client  *derphttp.Client
	packets chan jointPacket
}
type jointPacket struct {
	source key.NodePublic
	data   []byte
	err    error
}

func (h *jointHarness) client(t *testing.T, url, name string, allow bool) *jointClient {
	t.Helper()
	client, err := derphttp.NewClient(h.keys[name], url, t.Logf, netmon.NewStatic())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := client.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	verifyTailnetHTTPAdmission(t, client, allow)
	if !allow {
		return nil
	}
	c := &jointClient{client: client, packets: make(chan jointPacket, 32)}
	done := make(chan struct{})
	stopped := make(chan struct{})
	t.Cleanup(func() { close(done); client.Close(); <-stopped })
	go func() {
		defer close(stopped)
		for {
			msg, err := client.Recv()
			if err != nil {
				select {
				case c.packets <- jointPacket{err: err}:
				case <-done:
				}
				return
			}
			if packet, ok := msg.(derp.ReceivedPacket); ok {
				result := jointPacket{source: packet.Source, data: append([]byte(nil), packet.Data...)}
				select {
				case c.packets <- result:
				case <-done:
					return
				}
			}
		}
	}()
	return c
}

func (h *jointHarness) send(t *testing.T, from *jointClient, name string, number byte, size int) {
	t.Helper()
	if err := from.client.Send(h.keys[name].Public(), bytes.Repeat([]byte{number}, size)); err != nil {
		t.Fatal(err)
	}
}

func (h *jointHarness) receive(t *testing.T, to *jointClient, from string, number byte, size int) {
	t.Helper()
	select {
	case got := <-to.packets:
		if got.err != nil {
			t.Fatal(got.err)
		}
		if got.source != h.keys[from].Public() || !bytes.Equal(got.data, bytes.Repeat([]byte{number}, size)) {
			t.Fatalf("unexpected numbered packet: source=%v length=%d", got.source, len(got.data))
		}
	case <-time.After(3 * time.Second):
		t.Fatal("expected numbered packet missed receive deadline")
	}
}

func jointAbsent(t *testing.T, to *jointClient) {
	t.Helper()
	select {
	case packet := <-to.packets:
		t.Fatalf("expected policing drop, got packet/error: %+v", packet)
	case <-time.After(300 * time.Millisecond):
	}
}

func (h *jointHarness) tenSubjects(t *testing.T) {
	url := h.server(t, h.ready.Normal, jointConfig(1, 1), true)
	for i := 0; i < 10; i++ {
		name := fmt.Sprintf("node%d", i)
		client := h.client(t, url, name, true)
		h.send(t, client, name, byte(i+1), derp.MaxPacketSize)
		h.receive(t, client, name, byte(i+1), derp.MaxPacketSize)
		h.send(t, client, name, byte(i+20), derp.MaxPacketSize)
		jointAbsent(t, client)
	}
}

func (h *jointHarness) policing(t *testing.T, direction string) {
	up, down := 1, 1
	if direction == "upload" {
		down = 0
	}
	if direction == "download" {
		up = 0
	}
	url := h.server(t, h.ready.Normal, jointConfig(up, down), true)
	first, other, free := h.client(t, url, "node0", true), h.client(t, url, "other", true), h.client(t, url, "free", true)
	if direction == "download" {
		h.send(t, free, "node0", 1, derp.MaxPacketSize)
		h.receive(t, first, "free", 1, derp.MaxPacketSize)
		h.send(t, free, "other", 2, derp.MaxPacketSize)
		jointAbsent(t, other)
		h.send(t, other, "free", 3, derp.MaxPacketSize)
		h.receive(t, free, "other", 3, derp.MaxPacketSize)
	} else {
		destination, receiver := "free", free
		if direction == "both" {
			destination, receiver = "other", other
		}
		h.send(t, first, destination, 1, derp.MaxPacketSize)
		h.receive(t, receiver, "node0", 1, derp.MaxPacketSize)
		h.send(t, other, "free", 2, derp.MaxPacketSize)
		jointAbsent(t, free)
		if direction == "both" {
			h.send(t, free, "other", 3, derp.MaxPacketSize)
			jointAbsent(t, other)
		}
		if direction == "upload" {
			h.send(t, free, "other", 4, derp.MaxPacketSize)
			h.receive(t, other, "free", 4, derp.MaxPacketSize)
		}
	}
	// A full-size independent control proves the override bypasses both default buckets.
	h.send(t, free, "free", 9, derp.MaxPacketSize)
	h.receive(t, free, "free", 9, derp.MaxPacketSize)
	h.send(t, free, "free", 10, derp.MaxPacketSize)
	h.receive(t, free, "free", 10, derp.MaxPacketSize)
}
