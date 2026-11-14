package derpserver

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"

	"tailscale.com/derp"
	"tailscale.com/types/key"
)

func newAccountTCPHarness(t *testing.T, config string, identities map[key.NodePublic]tailnetAccountIdentity, responseVersion int) *tailnetTCPHarness {
	t.Helper()
	h := &tailnetTCPHarness{server: New(key.NewNode(), t.Logf), configPath: filepath.Join(t.TempDir(), "rates.json")}
	callback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.calls.Add(1)
		var req tailnetAdmissionRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		if req.IdentityVersion != 2 || req.Source.String() != "127.0.0.1" {
			t.Error("wrong account request", req.IdentityVersion, req.Source)
		}
		id, ok := identities[req.NodePublic]
		if !ok {
			fmt.Fprint(w, `{"Allow":false}`)
			return
		}
		fmt.Fprintf(w, `{"Allow":true,"IdentityVersion":%d,"Identity":{"NodePublic":%q,"RateLimitSubject":%q,"Account":{"Kind":%q,"Key":%q}}}`, responseVersion, req.NodePublic.String(), id.network, id.kind, id.key)
	}))
	t.Cleanup(callback.Close)
	h.server.SetVerifyClientURL(callback.URL)
	h.server.SetVerifyClientURLFailOpen(false)
	if err := h.server.SetTailnetAccountIdentity(true); err != nil {
		t.Fatal(err)
	}
	h.reload(t, config)
	startAccountTCP(t, h)
	return h
}

func startAccountTCP(t *testing.T, h *tailnetTCPHarness) {
	t.Helper()
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
}

func accountTCPConfig(parent, child int, direction string) string {
	up, down := 0, 0
	if direction == "upload" {
		up = child
	} else {
		down = child
	}
	network := fmt.Sprintf(`{"upload_bytes_per_second":%d,"download_bytes_per_second":%d,"accounts":{"default":%s}}`, parent, parent, accountLeaf(up, down))
	return `{"tailnets":{"network":` + network + `,"other":` + network + `,"free":` + accountLeaf(0, 0) + `}}`
}

// Distinct private keys authenticate the same account; every direction charges
// the local endpoint, not the remote sender's identity on download.
func TestAccountTCPPayloadPolicing(t *testing.T) {
	for _, direction := range []string{"upload", "download"} {
		t.Run(direction, func(t *testing.T) {
			a, b, c, tag1, tag2, other, free := key.NewNode(), key.NewNode(), key.NewNode(), key.NewNode(), key.NewNode(), key.NewNode(), key.NewNode()
			identities := map[key.NodePublic]tailnetAccountIdentity{a.Public(): userID("network", "a"), b.Public(): userID("network", "a"), c.Public(): userID("network", "b"), tag1.Public(): userID("network", "tagged-registrant"), tag2.Public(): userID("network", "tagged-registrant"), other.Public(): userID("other", "a"), free.Public(): userID("free", "f")}
			h := newAccountTCPHarness(t, accountTCPConfig(0, 1, direction), identities, 2)
			ca, cb, cc, ct1, ct2, co, cf := h.connect(t, a, true), h.connect(t, b, true), h.connect(t, c, true), h.connect(t, tag1, true), h.connect(t, tag2, true), h.connect(t, other, true), h.connect(t, free, true)
			sequence := byte(0)
			send := func(client *tailnetTCPClient, node key.NodePublic, deliver bool) {
				t.Helper()
				sequence++
				payload := bytes.Repeat([]byte{sequence}, derp.MaxPacketSize)
				if direction == "upload" {
					tailnetSend(t, client, free.Public(), payload)
					if deliver {
						tailnetRecv(t, cf, node, payload)
					}
				} else {
					tailnetSend(t, cf, node, payload)
					if deliver {
						tailnetRecv(t, client, free.Public(), payload)
					}
				}
			}
			send(ca, a.Public(), true)
			send(cb, b.Public(), false)
			dropped := func() int64 {
				if direction == "upload" {
					return h.server.userRateUploadDropped.Value()
				}
				return h.server.userRateDownloadDropped.Value()
			}
			tailnetAwait(t, func() bool { return dropped() == 1 })
			send(cc, c.Public(), true)
			send(co, other.Public(), true)
			send(ct1, tag1.Public(), true)
			send(ct2, tag2.Public(), false)
			tailnetAwait(t, func() bool { return dropped() == 2 })
			// Numbered reachable control on an independent identity, on the same server.
			tailnetSend(t, cf, free.Public(), []byte("control-2"))
			tailnetRecv(t, cf, free.Public(), []byte("control-2"))
			// Re-enable delivery on the dropped sockets: a leaked numbered
			// packet would precede this control and fail the exact receive.
			h.reload(t, tailnetTCPConfig(0, 0))
			for _, client := range []*tailnetTCPClient{cb, ct2} {
				tailnetSend(t, cf, client.PublicKey(), []byte("delivery-control-3"))
				tailnetRecv(t, client, free.Public(), []byte("delivery-control-3"))
			}
			if h.calls.Load() != 7 {
				t.Fatal("not admitted once per connection")
			}
		})
	}
}

func TestAccountTCPParentCompetition(t *testing.T) {
	for _, direction := range []string{"upload", "download"} {
		t.Run(direction, func(t *testing.T) {
			a, b, free := key.NewNode(), key.NewNode(), key.NewNode()
			h := newAccountTCPHarness(t, accountTCPConfig(1, 0, direction), map[key.NodePublic]tailnetAccountIdentity{a.Public(): userID("network", "a"), b.Public(): userID("network", "b"), free.Public(): userID("free", "f")}, 2)
			ca, cb, cf := h.connect(t, a, true), h.connect(t, b, true), h.connect(t, free, true)
			payload := bytes.Repeat([]byte{3}, derp.MaxPacketSize)
			if direction == "upload" {
				tailnetSend(t, ca, free.Public(), payload)
				tailnetRecv(t, cf, a.Public(), payload)
				tailnetSend(t, cb, free.Public(), payload)
				tailnetAwait(t, func() bool { return h.server.userRateUploadDropped.Value() == 1 })
			} else {
				tailnetSend(t, cf, a.Public(), payload)
				tailnetRecv(t, ca, free.Public(), payload)
				tailnetSend(t, cf, b.Public(), payload)
				tailnetAwait(t, func() bool { return h.server.userRateDownloadDropped.Value() == 1 })
			}
			tailnetSend(t, cf, free.Public(), []byte("parent-control"))
			tailnetRecv(t, cf, free.Public(), []byte("parent-control"))
		})
	}
}

func TestAccountTCPAdmissionAndReload(t *testing.T) {
	a, free := key.NewNode(), key.NewNode()
	ids := map[key.NodePublic]tailnetAccountIdentity{a.Public(): userID("network", "a"), free.Public(): userID("free", "f")}
	t.Run("v2 required even without accounts", func(t *testing.T) {
		h := newAccountTCPHarness(t, tailnetTCPConfig(0, 0), ids, 1)
		h.connect(t, a, false)
		if h.calls.Load() != 1 {
			t.Fatal("no callback")
		}
	})
	t.Run("unknown identity rejected", func(t *testing.T) {
		h := newAccountTCPHarness(t, tailnetTCPConfig(0, 0), map[key.NodePublic]tailnetAccountIdentity{a.Public(): accountID("network", "unknown", "")}, 2)
		h.connect(t, a, false)
	})
	t.Run("live reload", func(t *testing.T) {
		h := newAccountTCPHarness(t, tailnetTCPConfig(0, 0), ids, 2)
		ca, cf := h.connect(t, a, true), h.connect(t, free, true)
		// Registered while unlimited, before any packet. Adding limits starts at 0.
		h.reload(t, accountTCPConfig(0, 1, "upload"))
		tailnetSend(t, ca, free.Public(), []byte("limited-1"))
		tailnetAwait(t, func() bool { return h.server.userRateUploadDropped.Value() == 1 })
		h.reload(t, tailnetTCPConfig(0, 0))
		tailnetSend(t, ca, free.Public(), []byte("disabled-2"))
		tailnetRecv(t, cf, a.Public(), []byte("disabled-2"))
		h.reload(t, accountTCPConfig(0, 1, "upload"))
		ca.conn.Close()
		ca = h.connect(t, a, true)
		tailnetSend(t, ca, free.Public(), bytes.Repeat([]byte{4}, derp.MaxPacketSize))
		tailnetAwait(t, func() bool { return h.server.userRateUploadDropped.Value() == 2 })
		writeAccountPolicy(t, h.configPath, accountConfig("0", `{"upload_bytes_per_second":1}`))
		if h.server.LoadAndApplyTailnetRateConfig(h.configPath) == nil {
			t.Fatal("invalid reload accepted")
		}
		tailnetSend(t, ca, free.Public(), bytes.Repeat([]byte{5}, derp.MaxPacketSize))
		tailnetAwait(t, func() bool { return h.server.userRateUploadDropped.Value() == 3 })
		h.reload(t, `{"tailnets":{"free":`+accountLeaf(0, 0)+`}}`)
		tailnetSend(t, ca, free.Public(), []byte("denied-3"))
		tailnetAwait(t, func() bool { return h.server.userRateUploadDropped.Value() == 4 })
		h.connect(t, a, false)
		h.reload(t, tailnetTCPConfig(0, 0))
		tailnetSend(t, ca, free.Public(), []byte("restored-4"))
		tailnetRecv(t, cf, a.Public(), []byte("restored-4"))
		if h.calls.Load() != 4 {
			t.Fatal("reload changed identity")
		}
	})
}

func TestAccountTCPMesh(t *testing.T) {
	a := key.NewNode()
	h := newAccountTCPHarness(t, accountTCPConfig(0, 1, "download"), map[key.NodePublic]tailnetAccountIdentity{a.Public(): userID("network", "a")}, 2)
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
	payload := bytes.Repeat([]byte{6}, derp.MaxPacketSize)
	for count := 0; count < 2; count++ {
		if err := mesh.ForwardPacket(remote, a.Public(), derp.LoanBytes(payload)); err != nil {
			t.Fatal(err)
		}
		if count == 0 {
			tailnetRecv(t, ca, remote, payload)
		}
	}
	tailnetAwait(t, func() bool { return h.server.userRateDownloadDropped.Value() == 1 })
	if h.server.userRateUploadDropped.Value() != 0 || h.calls.Load() != 1 {
		t.Fatal("mesh charged upload or required account")
	}
	tailnetSend(t, ca, mesh.PublicKey(), []byte("mesh-control"))
	tailnetRecv(t, mesh, a.Public(), []byte("mesh-control"))
}
