package derpserver

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"tailscale.com/derp"
	"tailscale.com/types/key"
)

func TestAccountPolicySelection(t *testing.T) {
	a, b := userID("network", "a"), userID("network", "b")
	tagged := accountID("network", "tagged", "shared")
	leaf := accountLeaf(10, 20)
	parent := `{"upload_bytes_per_second":0,"download_bytes_per_second":0,"accounts":{"default":` + leaf + `,"tagged":` + accountLeaf(30, 40) + `,"overrides":{"` + a.key + `":` + accountLeaf(0, 50) + `}}}`
	cfg := parseAccountConfig(t, `{"default":`+parent+`,"tailnets":{"explicit":`+accountLeaf(0, 0)+`}}`)
	for _, tc := range []struct {
		id       tailnetAccountIdentity
		up, down int
	}{{a, 0, 50}, {b, 10, 20}, {tagged, 30, 40}, {userID("explicit", "a"), 0, 0}} {
		p, ok := cfg.accountPolicyFor(tc.id)
		if !ok || p.upload != tc.up || p.download != tc.down {
			t.Fatalf("incorrect leaf selection for %s: %v", tc.id.kind, p)
		}
	}
	r := newTailnetRateRegistry(cfg, func() time.Time { return time.Unix(1, 0) })
	for _, id := range []tailnetAccountIdentity{a, b, tagged} {
		r.admitAccount(id)
	}
	if !r.allowAccount(a, 2*derp.MaxPacketSize, true) {
		t.Fatal("override zero did not replace finite default")
	}
	if r.allowAccount(b, 2*derp.MaxPacketSize, true) || r.allowAccount(tagged, 2*derp.MaxPacketSize, true) {
		t.Fatal("override leaked to other identities")
	}
	// An explicit network never inherits accounts from the top-level default.
	explicit := userID("explicit", "a")
	r.admitAccount(explicit)
	if !r.allowAccount(explicit, 2*derp.MaxPacketSize, true) {
		t.Fatal("accounts inherited across complete network override")
	}
}

func TestAccountFiniteReloadLifecycle(t *testing.T) {
	for _, upload := range []bool{true, false} {
		t.Run(fmt.Sprint(upload), func(t *testing.T) {
			now := time.Unix(1, 0)
			config := func(rate, burst int) tailnetRateConfig {
				leaf := fmt.Sprintf(`{"upload_bytes_per_second":%d,"download_bytes_per_second":%d,"burst_bytes":%d}`, rate, rate, burst)
				return parseAccountConfig(t, accountConfig("0", leaf))
			}
			burst := derp.MaxPacketSize
			cfg := config(10, 2*burst)
			r := newTailnetRateRegistry(cfg, func() time.Time { return now })
			id := userID("network", "a")
			if !r.admitAccount(id) {
				t.Fatal("admit")
			}
			if !r.allowAccount(id, 2*burst-5, upload) {
				t.Fatal("first limited should start full")
			}
			now = now.Add(1500 * time.Millisecond)
			r.apply(config(100, 3*burst)) // settle old 10/sec: 5+15, not new 100/sec
			b := directionalBucket(r.accounts[id], upload)
			if b.tokens != 20 {
				t.Fatalf("old rate settlement/growth = %v want 20", b.tokens)
			}
			r.apply(config(100, 3*burst))
			r.admitAccount(id)
			if b.tokens != 20 {
				t.Fatal("same reload/reconnect refilled")
			}
			now = now.Add(time.Hour)
			r.apply(config(100, burst))
			if b.tokens != float64(burst) {
				t.Fatal("shrink cap")
			}
			if !r.allowAccount(id, burst-7, upload) {
				t.Fatal("spend before freeze")
			}
			r.apply(config(0, burst))
			now = now.Add(time.Hour)
			if !r.allowAccount(id, 2*burst, upload) {
				t.Fatal("unlimited did not bypass child")
			}
			r.apply(config(10, burst))
			if b.tokens != 7 {
				t.Fatal("unlimited accrued")
			}
			r.apply(parseAccountConfig(t, `{"tailnets":{}}`))
			now = now.Add(time.Hour)
			if r.allowAccount(id, 0, upload) {
				t.Fatal("parent deny revived by child")
			}
			r.admitAccount(id)
			r.apply(config(10, burst))
			if !r.allowAccount(id, 7, upload) || r.allowAccount(id, 1, upload) {
				t.Fatal("deny/reconnect reset balance")
			}
			if len(r.accounts) != 1 {
				t.Fatal("history lost")
			}
		})
	}
}

func TestAccountInvalidReloadAtomic(t *testing.T) {
	cfg := accountConfig("1", accountLeaf(1, 1))
	s := New(key.NewNode(), t.Logf)
	defer s.Close()
	s.SetVerifyClientURL("http://synthetic.invalid")
	s.SetVerifyClientURLFailOpen(false)
	if err := s.SetTailnetAccountIdentity(true); err != nil {
		t.Fatal(err)
	}
	path := t.TempDir() + "/rates.json"
	writeAccountPolicy(t, path, cfg)
	if err := s.LoadAndApplyTailnetRateConfig(path); err != nil {
		t.Fatal(err)
	}
	id := userID("network", "a")
	s.tailnetRate.admitAccount(id)
	before := s.tailnetRate.config.policies["network"].accounts
	// The valid first network must not partially apply if a later policy is bad.
	invalid := strings.TrimSuffix(cfg, `}}`) + `,"z-invalid":{"upload_bytes_per_second":9}}}`
	writeAccountPolicy(t, path, invalid)
	if s.LoadAndApplyTailnetRateConfig(path) == nil {
		t.Fatal("invalid reload succeeded")
	}
	if s.tailnetRate.config.policies["network"].accounts != before {
		t.Fatal("partial config publication")
	}
	if !s.tailnetRate.allowAccount(id, derp.MaxPacketSize, true) {
		t.Fatal("invalid reload spent or changed balances")
	}
}

func TestAccountLeafValidation(t *testing.T) {
	leaf := accountLeaf(1, 1)
	for _, slot := range []string{"default", "tagged", "overrides"} {
		for _, value := range []string{`null`, `"1"`, `-1`, `1.0`, `1e0`, `2147483648`, `true`} {
			t.Run(slot+"/"+value, func(t *testing.T) {
				bad := strings.Replace(leaf, `"upload_bytes_per_second":1`, `"upload_bytes_per_second":`+value, 1)
				accounts := `{"default":` + leaf + `,"tagged":` + leaf + `}`
				if slot == "overrides" {
					accounts = strings.TrimSuffix(accounts, `}`) + `,"overrides":{"` + userID("network", "a").key + `":` + bad + `}}`
				} else {
					accounts = strings.Replace(accounts, `"`+slot+`":`+leaf, `"`+slot+`":`+bad, 1)
				}
				if _, err := parseTailnetRateConfig([]byte(`{"tailnets":{"network":{"upload_bytes_per_second":0,"download_bytes_per_second":0,"accounts":` + accounts + `}}}`)); err == nil {
					t.Fatal("invalid leaf accepted")
				}
			})
		}
	}
	for _, burst := range []int{0, derp.MaxPacketSize - 1, derp.MaxPacketSize, 2147483647} {
		t.Run(fmt.Sprint("burst/", burst), func(t *testing.T) {
			child := strings.TrimSuffix(leaf, `}`) + fmt.Sprintf(`,"burst_bytes":%d}`, burst)
			_, err := parseTailnetRateConfig([]byte(accountConfig("0", child)))
			if (err == nil) != (burst >= derp.MaxPacketSize) {
				t.Fatal("leaf burst contract", err)
			}
		})
	}
	for _, bad := range []string{`{"upload_bytes_per_second":1,"upload_bytes_per_second":1,"download_bytes_per_second":1}`, `{"upload_bytes_per_second":1,"upload_bytes_per_second\u0000":1,"download_bytes_per_second":1}`, `{"upload_bytes_per_second":1,"download_bytes_per_second":1,"download_bytes_per_second":1}`} {
		if _, err := parseTailnetRateConfig([]byte(accountConfig("0", bad))); err == nil {
			t.Fatal("duplicate/unknown leaf accepted")
		}
	}
}
