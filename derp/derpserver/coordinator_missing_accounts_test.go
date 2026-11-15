package derpserver

import (
	"testing"
	"time"

	"tailscale.com/derp"
)

// An absent static child means unlimited, not a zero-capacity dynamic bucket.
func TestCoordinatorAccountOverlayWithoutStaticAccounts(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		t.Run(map[bool]string{false: "absent", true: "explicit_burst"}[explicit], func(t *testing.T) {
			now := time.Unix(1, 0)
			config := `{"tailnets":{"network":{"upload_bytes_per_second":0,"download_bytes_per_second":0}}}`
			if explicit {
				config = accountConfig("0", `{"upload_bytes_per_second":0,"download_bytes_per_second":0,"burst_bytes":131072}`)
			}
			r := newTailnetRateRegistry(parseAccountConfig(t, config), func() time.Time { return now })
			id := userID("network", "a")
			if !r.admitAccount(id) {
				t.Fatal("admission")
			}
			r.enableCoordinator()
			applyCoordinator(t, r, allocationJSON(id.key, `{"mode":"limited","rate":65536}`, `{"mode":"limited","rate":65536}`))
			burst := derp.MaxPacketSize
			if explicit {
				burst = 131072
			}
			if r.accounts[id].upload.burst != burst {
				t.Fatalf("capacity=%d want %d", r.accounts[id].upload.burst, burst)
			}
			if r.allowAccount(id, derp.MaxPacketSize, true) {
				t.Fatal("first finite overlay gifted burst")
			}
			now = now.Add(time.Second)
			for _, upload := range []bool{true, false} {
				if !r.allowAccount(id, derp.MaxPacketSize, upload) {
					t.Fatal("finite overlay permanently denied full payload")
				}
			}
			r.apply(parseAccountConfig(t, config))
			if r.allowAccount(id, derp.MaxPacketSize, true) {
				t.Fatal("static reload gifted burst")
			}
			now = now.Add(time.Second)
			if !r.allowAccount(id, derp.MaxPacketSize, true) {
				t.Fatal("static reload lost dynamic capacity")
			}
		})
	}
}
