package derpserver

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"tailscale.com/derp"
)

func accountID(network, kind, key string) tailnetAccountIdentity {
	return tailnetAccountIdentity{network, kind, key}
}
func userID(network string, digit string) tailnetAccountIdentity {
	return accountID(network, "user", digit+"@synthetic.test")
}
func parseAccountConfig(t *testing.T, input string) tailnetRateConfig {
	t.Helper()
	cfg, err := parseTailnetRateConfig([]byte(input))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// Given competing accounts, a rejected debit must not consume either layer.
func TestAccountAtomicDebit(t *testing.T) {
	for _, upload := range []bool{true, false} {
		t.Run(map[bool]string{true: "upload", false: "download"}[upload], func(t *testing.T) {
			now := time.Unix(1, 0)
			r := newTailnetRateRegistry(parseAccountConfig(t, accountConfig("1", accountLeaf(1, 1))), func() time.Time { return now })
			a, b := userID("network", "a"), userID("network", "b")
			if !r.admitAccount(a) || !r.admitAccount(b) {
				t.Fatal("admit")
			}
			// Empty child A, leaving the parent full: child rejection cannot debit parent.
			child := &r.accounts[a].upload
			if !upload {
				child = &r.accounts[a].download
			}
			child.tokens = 0
			if r.allowAccount(a, 1, upload) {
				t.Fatal("empty child admitted")
			}
			if !r.allowAccount(b, derp.MaxPacketSize, upload) {
				t.Fatal("child rejection spent parent")
			}
			child.tokens = derp.MaxPacketSize
			if r.allowAccount(a, 1, upload) {
				t.Fatal("empty parent admitted")
			}
			if child.tokens != derp.MaxPacketSize {
				t.Fatal("parent rejection spent child")
			}
			if r.allowAccount(a, -1, upload) || r.allowAccount(userID("network", "c"), 0, upload) {
				t.Fatal("invalid debit")
			}
		})
	}
}

func TestAccountIsolationAndZero(t *testing.T) {
	for _, rates := range [][2]int{{1, 0}, {0, 1}, {0, 0}, {1, 1}} {
		t.Run(fmt.Sprintf("parent=%d-child=%d", rates[0], rates[1]), func(t *testing.T) {
			cfg := parseAccountConfig(t, accountConfig(fmt.Sprint(rates[0]), accountLeaf(rates[1], rates[1])))
			cfg.defaultPolicy = new(tailnetRatePolicy)
			*cfg.defaultPolicy = cfg.policies["network"]
			r := newTailnetRateRegistry(cfg, func() time.Time { return time.Unix(1, 0) })
			a, b, other, tag := userID("network", "a"), userID("network", "b"), userID("other", "a"), userID("network", "tagged-registrant")
			for _, id := range []tailnetAccountIdentity{a, b, other, tag} {
				if !r.admitAccount(id) {
					t.Fatal("admit")
				}
			}
			for _, upload := range []bool{true, false} {
				if !r.allowAccount(a, derp.MaxPacketSize, upload) {
					t.Fatal("first burst")
				}
				if got := r.allowAccount(a, derp.MaxPacketSize, upload); got != (rates == [2]int{0, 0}) {
					t.Fatal("same account burst", got)
				}
				if got := r.allowAccount(b, derp.MaxPacketSize, upload); got != (rates[0] == 0) {
					t.Fatal("parent sharing", got)
				}
				if !r.allowAccount(other, derp.MaxPacketSize, upload) {
					t.Fatal("cross network leaked")
				}
				if got := r.allowAccount(tag, derp.MaxPacketSize, upload); got != (rates[0] == 0) {
					t.Fatal("tagged", got)
				}
			}
		})
	}
}

// Given previously admitted unlimited/denied identities, enabling child limits
// cannot mint their first burst. Disabling preserves finite history too.
func TestAccountReloadHistory(t *testing.T) {
	for _, initial := range []string{`{"tailnets":{}}`, `{"tailnets":{"network":` + accountLeaf(0, 0) + `}}`} {
		t.Run(initial, func(t *testing.T) {
			now := time.Unix(1, 0)
			r := newTailnetRateRegistry(parseAccountConfig(t, initial), func() time.Time { return now })
			id := userID("network", "a")
			r.admitAccount(id)
			limited := parseAccountConfig(t, accountConfig("0", accountLeaf(10, 20)))
			r.apply(limited)
			if r.allowAccount(id, 1, true) || r.allowAccount(id, 1, false) {
				t.Fatal("history got first finite burst")
			}
			now = now.Add(time.Second)
			if !r.allowAccount(id, 4, true) || !r.allowAccount(id, 4, false) {
				t.Fatal("refill")
			}
			r.apply(limited)
			r.admitAccount(id)
			if r.accounts[id].upload.tokens != 6 || r.accounts[id].download.tokens != 16 {
				t.Fatal("same reload/reconnect refilled")
			}
			r.apply(parseAccountConfig(t, `{"tailnets":{"network":`+accountLeaf(0, 0)+`}}`))
			now = now.Add(time.Hour)
			r.apply(limited)
			if !r.allowAccount(id, 6, true) || r.allowAccount(id, 1, true) {
				t.Fatal("disabled child did not freeze")
			}
			r.apply(parseAccountConfig(t, `{"tailnets":{}}`))
			now = now.Add(time.Hour)
			r.apply(limited)
			if r.allowAccount(id, 1, true) {
				t.Fatal("deny did not freeze")
			}
			if !r.allowAccount(id, 16, false) || r.allowAccount(id, 1, false) {
				t.Fatal("download history")
			}
		})
	}
}

func TestAccountConcurrentRegistry(t *testing.T) {
	cfg := parseAccountConfig(t, accountConfig("100", accountLeaf(10, 20)))
	r := newTailnetRateRegistry(cfg, func() time.Time { return time.Unix(1, 0) })
	var wg sync.WaitGroup
	var uploads, downloads atomic.Int64
	for index := 0; index < 8; index++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id := userID("network", "a")
			for count := 0; count < 2000; count++ {
				r.admitAccount(id)
				if r.allowAccount(id, 7, true) {
					uploads.Add(1)
				}
				if r.allowAccount(id, 7, false) {
					downloads.Add(1)
				}
				r.apply(cfg)
			}
		}()
	}
	wg.Wait()
	want := int64(derp.MaxPacketSize / 7)
	if uploads.Load() != want || downloads.Load() != want {
		t.Fatalf("nonlinearizable debit/reload: up=%d down=%d want=%d", uploads.Load(), downloads.Load(), want)
	}
}
