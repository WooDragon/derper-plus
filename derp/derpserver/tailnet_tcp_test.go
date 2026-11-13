// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

package derpserver

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"tailscale.com/derp"
	"tailscale.com/types/key"
)

type tailnetTCPHarness struct {
	server     *Server
	addr       string
	configPath string
	calls      atomic.Int64
}

func newTailnetTCPHarness(t *testing.T, config string, subjects map[key.NodePublic]string) *tailnetTCPHarness {
	t.Helper()
	h := &tailnetTCPHarness{server: New(key.NewNode(), t.Logf), configPath: filepath.Join(t.TempDir(), "rates.json")}
	callback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.calls.Add(1)
		var req tailnetAdmissionRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			http.Error(w, "bad request", 400)
			return
		}
		if req.IdentityVersion != 1 || req.Source.String() != "127.0.0.1" {
			t.Error("invalid identity request", req)
		}
		subject, ok := subjects[req.NodePublic]
		if !ok {
			fmt.Fprint(w, `{"Allow":false}`)
			return
		}
		fmt.Fprintf(w, `{"Allow":true,"IdentityVersion":1,"Identity":{"NodePublic":%q,"RateLimitSubject":%q}}`, req.NodePublic.String(), subject)
	}))
	t.Cleanup(callback.Close)
	h.server.SetVerifyClientURL(callback.URL)
	h.server.SetVerifyClientURLFailOpen(false)
	h.reload(t, config)
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	h.addr = ln.Addr().String()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				h.server.Accept(context.Background(), conn, bufio.NewReadWriter(bufio.NewReader(conn), bufio.NewWriter(conn)), conn.RemoteAddr().String())
			}()
		}
	}()
	t.Cleanup(func() { ln.Close(); h.server.Close(); wg.Wait() })
	return h
}

func (h *tailnetTCPHarness) reload(t *testing.T, config string) {
	t.Helper()
	if err := os.WriteFile(h.configPath, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	if err := h.server.LoadAndApplyTailnetRateConfig(h.configPath); err != nil {
		t.Fatal(err)
	}
}

type tailnetTCPClient struct {
	*derp.Client
	conn net.Conn
}

func (h *tailnetTCPHarness) connect(t *testing.T, private key.NodePrivate, allow bool, opts ...derp.ClientOpt) *tailnetTCPClient {
	t.Helper()
	conn, err := net.DialTimeout("tcp4", h.addr, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	conn.SetDeadline(time.Now().Add(3 * time.Second))
	client, err := derp.NewClient(private, conn, bufio.NewReadWriter(bufio.NewReader(conn), bufio.NewWriter(conn)), t.Logf, opts...)
	if err != nil {
		t.Fatal(err)
	}
	msg, err := client.Recv()
	if !allow {
		if err == nil {
			t.Fatalf("denied client received %T", msg)
		}
		var timeout net.Error
		if errors.As(err, &timeout) && timeout.Timeout() {
			t.Fatal("denial timed out instead of rejecting")
		}
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := msg.(derp.ServerInfoMessage); !ok {
		t.Fatalf("admission missing ServerInfoMessage: %T", msg)
	}
	conn.SetDeadline(time.Time{})
	return &tailnetTCPClient{client, conn}
}

func tailnetSend(t *testing.T, from *tailnetTCPClient, to key.NodePublic, payload []byte) {
	t.Helper()
	if err := from.Send(to, payload); err != nil {
		t.Fatal(err)
	}
}

func tailnetRecv(t *testing.T, to *tailnetTCPClient, from key.NodePublic, want []byte) {
	t.Helper()
	to.conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	defer to.conn.SetReadDeadline(time.Time{})
	for {
		msg, err := to.Recv()
		if err != nil {
			t.Fatal(err)
		}
		if packet, ok := msg.(derp.ReceivedPacket); ok {
			if packet.Source != from || !bytes.Equal(packet.Data, want) {
				t.Fatalf("unexpected packet from %v, length %d", packet.Source, len(packet.Data))
			}
			return
		}
	}
}

func tailnetAwait(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for !condition() {
		select {
		case <-deadline.C:
			t.Fatal("observable state did not converge")
		case <-ticker.C:
		}
	}
}

func tailnetTCPConfig(upload, download int) string {
	return fmt.Sprintf(`{"default":{"upload_bytes_per_second":%d,"download_bytes_per_second":%d},"tailnets":{"free":{"upload_bytes_per_second":0,"download_bytes_per_second":0}}}`, upload, download)
}

// Given two distinct private keys in one tailnet and an independent control,
// exhausting either direction drops payloads without breaking the live socket.
func TestTailnetTCPPayloadPolicing(t *testing.T) {
	for _, direction := range []string{"upload", "download", "both"} {
		t.Run(direction, func(t *testing.T) {
			a, b, free := key.NewNode(), key.NewNode(), key.NewNode()
			up, down := 0, 0
			if direction != "download" {
				up = 1
			}
			if direction != "upload" {
				down = 1
			}
			h := newTailnetTCPHarness(t, tailnetTCPConfig(up, down), map[key.NodePublic]string{a.Public(): "network", b.Public(): "network", free.Public(): "free"})
			ca, cb, cf := h.connect(t, a, true), h.connect(t, b, true), h.connect(t, free, true)
			payload := bytes.Repeat([]byte{1}, derp.MaxPacketSize)
			if direction == "download" {
				tailnetSend(t, cf, a.Public(), payload)
				tailnetRecv(t, ca, free.Public(), payload)
				tailnetSend(t, cf, b.Public(), payload)
				tailnetAwait(t, func() bool { return h.server.userRateDownloadDropped.Value() == 1 })
				tailnetSend(t, cb, free.Public(), []byte("download-only-control"))
				tailnetRecv(t, cf, b.Public(), []byte("download-only-control"))
			} else {
				dst := free.Public()
				receiver := cf
				if direction == "both" {
					dst = b.Public()
					receiver = cb
				}
				tailnetSend(t, ca, dst, payload)
				tailnetRecv(t, receiver, a.Public(), payload)
				tailnetSend(t, cb, free.Public(), payload)
				tailnetAwait(t, func() bool { return h.server.userRateUploadDropped.Value() == 1 })
				if direction == "both" {
					tailnetSend(t, cf, b.Public(), payload)
					tailnetAwait(t, func() bool { return h.server.userRateDownloadDropped.Value() == 1 })
				}
				tailnetSend(t, cf, free.Public(), []byte("independent-control"))
				tailnetRecv(t, cf, free.Public(), []byte("independent-control"))
			}
			if h.calls.Load() != 3 {
				t.Fatal("identities not bound once at admission")
			}
		})
	}
}

func TestTailnetTCPMultipleConnections(t *testing.T) {
	a, b := key.NewNode(), key.NewNode()
	h := newTailnetTCPHarness(t, tailnetTCPConfig(1, 0), map[key.NodePublic]string{a.Public(): "network", b.Public(): "free"})
	ca, cb := h.connect(t, a, true), h.connect(t, b, true)
	payload := bytes.Repeat([]byte{6}, derp.MaxPacketSize)
	tailnetSend(t, ca, b.Public(), payload)
	tailnetRecv(t, cb, a.Public(), payload)
	// Keep the first socket open: another connection with the same node key
	// must share the subject's existing balance, just like a different node.
	other := h.connect(t, a, true)
	tailnetSend(t, other, b.Public(), payload)
	tailnetAwait(t, func() bool { return h.server.userRateUploadDropped.Value() == 1 })
	tailnetSend(t, cb, b.Public(), []byte("connection-control"))
	tailnetRecv(t, cb, b.Public(), []byte("connection-control"))
}

func TestTailnetTCPReloadAndReconnect(t *testing.T) {
	a, b := key.NewNode(), key.NewNode()
	h := newTailnetTCPHarness(t, tailnetTCPConfig(1, 0), map[key.NodePublic]string{a.Public(): "network", b.Public(): "free"})
	ca, cb := h.connect(t, a, true), h.connect(t, b, true)
	payload := bytes.Repeat([]byte{2}, derp.MaxPacketSize)
	tailnetSend(t, ca, b.Public(), payload)
	tailnetRecv(t, cb, a.Public(), payload)
	h.reload(t, tailnetTCPConfig(1, 0))
	ca.conn.Close()
	ca = h.connect(t, a, true)
	tailnetSend(t, ca, b.Public(), payload)
	tailnetAwait(t, func() bool { return h.server.userRateUploadDropped.Value() == 1 })
	h.reload(t, `{"tailnets":{}}`)
	tailnetSend(t, ca, b.Public(), []byte("deny-on-live-connection"))
	tailnetAwait(t, func() bool { return h.server.userRateUploadDropped.Value() == 2 })
	if h.calls.Load() != 3 {
		t.Fatal("reload re-identified established connections")
	}
	h.connect(t, a, false)
}

func TestTailnetTCPAdmissionHistory(t *testing.T) {
	for _, initial := range []string{`{"tailnets":{}}`, tailnetTCPConfig(0, 0)} {
		t.Run(initial, func(t *testing.T) {
			a, b := key.NewNode(), key.NewNode()
			h := newTailnetTCPHarness(t, initial, map[key.NodePublic]string{a.Public(): "network", b.Public(): "free"})
			allow := initial != `{"tailnets":{}}`
			h.connect(t, a, allow) // no payload: history must already exist
			h.reload(t, tailnetTCPConfig(1, 0))
			ca, cb := h.connect(t, a, true), h.connect(t, b, true)
			tailnetSend(t, ca, b.Public(), bytes.Repeat([]byte{3}, derp.MaxPacketSize))
			tailnetAwait(t, func() bool { return h.server.userRateUploadDropped.Value() == 1 })
			tailnetSend(t, cb, b.Public(), []byte("live-control"))
			tailnetRecv(t, cb, b.Public(), []byte("live-control"))
		})
	}
}

func TestTailnetTCPMeshAccounting(t *testing.T) {
	a := key.NewNode()
	h := newTailnetTCPHarness(t, tailnetTCPConfig(1, 1), map[key.NodePublic]string{a.Public(): "network"})
	if err := h.server.SetMeshKey(testMeshKey); err != nil {
		t.Fatal(err)
	}
	meshKey, err := key.ParseDERPMesh(testMeshKey)
	if err != nil {
		t.Fatal(err)
	}
	ca := h.connect(t, a, true)
	mesh := h.connect(t, key.NewNode(), true, derp.MeshKey(meshKey))
	remote := key.NewNode().Public()
	payload := bytes.Repeat([]byte{4}, derp.MaxPacketSize)
	if err := mesh.ForwardPacket(remote, a.Public(), derp.LoanBytes(payload)); err != nil {
		t.Fatal(err)
	}
	tailnetRecv(t, ca, remote, payload)
	if err := mesh.ForwardPacket(remote, a.Public(), derp.LoanBytes(payload)); err != nil {
		t.Fatal(err)
	}
	tailnetAwait(t, func() bool { return h.server.userRateDownloadDropped.Value() == 1 })
	if h.server.userRateUploadDropped.Value() != 0 || h.calls.Load() != 1 {
		t.Fatal("mesh was admitted or charged as client upload")
	}
	// Local upload is still untouched by forwarded ingress.
	tailnetSend(t, ca, mesh.PublicKey(), payload)
	tailnetRecv(t, mesh, a.Public(), payload)
}

func TestTailnetTCPProofOfPossession(t *testing.T) {
	claimed := key.NewNode()
	h := newTailnetTCPHarness(t, tailnetTCPConfig(1, 1), map[key.NodePublic]string{claimed.Public(): "network"})
	conn, err := net.DialTimeout("tcp4", h.addr, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(3 * time.Second))
	reader := bufio.NewReader(conn)
	size, err := derp.ReadFrameTypeHeader(reader, derp.FrameServerKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.CopyN(io.Discard, reader, int64(size)); err != nil {
		t.Fatal(err)
	}
	// Announce an allowed public key, but encrypt with an unrelated private key.
	encrypted := key.NewNode().SealTo(h.server.PublicKey(), []byte(`{"version":2}`))
	frame := claimed.Public().AppendTo(nil)
	frame = append(frame, encrypted...)
	writer := bufio.NewWriter(conn)
	if err := derp.WriteFrame(writer, derp.FrameClientInfo, frame); err != nil {
		t.Fatal(err)
	}
	if err := writer.Flush(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := derp.ReadFrameHeader(reader); err == nil {
		t.Fatal("forged key received a server frame")
	} else {
		var timeout net.Error
		if errors.As(err, &timeout) && timeout.Timeout() {
			t.Fatal("forged key timed out instead of rejecting")
		}
	}
	if h.calls.Load() != 0 {
		t.Fatal("unproven public key reached admission callback")
	}
	h.connect(t, claimed, true) // positive control really holds the allowed key
}
