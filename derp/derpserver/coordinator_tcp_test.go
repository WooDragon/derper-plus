package derpserver

import (
	"bytes"
	"testing"
	"time"

	"tailscale.com/types/key"
)

// Existing authenticated sockets use the unchanged DERP packet call sites.
// Dynamic deny, static reload and malformed responses never re-admit sockets.
func TestCoordinatorTCPPayloadAndReload(t *testing.T) {
	for _, upload := range []bool{true, false} {
		name := "download"
		if upload {
			name = "upload"
		}
		t.Run(name, func(t *testing.T) {
			account, free := key.NewNode(), key.NewNode()
			id, freeID := userID("network", "a"), userID("free", "free")
			config := accountTCPConfig(0, 0, name)
			h := newAccountTCPHarness(t, config, map[key.NodePublic]tailnetAccountIdentity{account.Public(): id, free.Public(): freeID}, 2)
			from, control := h.connect(t, account, true), h.connect(t, free, true)
			if err := h.server.EnableTailnetCoordinator(); err != nil {
				t.Fatal(err)
			}
			send := func(size int, deliver bool) {
				t.Helper()
				payload := bytes.Repeat([]byte{byte(size)}, size)
				if upload {
					tailnetSend(t, from, free.Public(), payload)
					if deliver {
						tailnetRecv(t, control, account.Public(), payload)
					}
				} else {
					tailnetSend(t, control, account.Public(), payload)
					if deliver {
						tailnetRecv(t, from, free.Public(), payload)
					}
				}
			}
			apply := func(subject string, deny bool) {
				t.Helper()
				up, down := `{"mode":"unlimited"}`, `{"mode":"unlimited"}`
				if deny {
					if upload {
						up = `{"mode":"deny"}`
					} else {
						down = `{"mode":"deny"}`
					}
				}
				if _, err := h.server.ApplyTailnetCoordinatorResponse(responseJSON(allocationJSON(subject, up, down)), "node", "boot", 1); err != nil {
					t.Fatal(err)
				}
			}
			dropped := func() int64 {
				if upload {
					return h.server.userRateUploadDropped.Value()
				}
				return h.server.userRateDownloadDropped.Value()
			}
			send(4, true)
			apply(id.key, true)
			send(5, false)
			tailnetAwait(t, func() bool { return dropped() == 1 })
			apply(id.key, false)
			apply("", true)
			send(6, false)
			tailnetAwait(t, func() bool { return dropped() == 2 })
			h.reload(t, config)
			if _, err := h.server.ApplyTailnetCoordinatorResponse([]byte(`{"version":1}`), "node", "boot", 1); err == nil {
				t.Fatal("malformed accepted")
			}
			send(8, false)
			tailnetAwait(t, func() bool { return dropped() == 3 })
			apply("", false)
			send(7, true)
			r := h.server.tailnetRate
			r.mu.Lock()
			r.coordinator.started = r.now().Add(-30 * time.Second)
			r.mu.Unlock()
			entries, _, err := h.server.TailnetCoordinatorSnapshot()
			if err != nil {
				t.Fatal(err)
			}
			found := 0
			for _, entry := range entries {
				if entry.Network != "network" {
					continue
				}
				found++
				direction := entry.Download
				if upload {
					direction = entry.Upload
				}
				if direction.Attempted != 30 || direction.Admitted != 11 {
					t.Fatalf("real packet accounting: %+v", direction)
				}
			}
			if found != 2 || h.calls.Load() != 2 {
				t.Fatal("identity re-admitted or statistics missing")
			}
		})
	}
}
