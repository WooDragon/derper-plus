package derpserver

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"tailscale.com/derp"
)

func TestCoordinatorModeIntersection(t *testing.T) {
	for _, static := range []struct {
		rate    int
		allowed bool
		name    string
	}{{0, false, "deny"}, {0, true, "unlimited"}, {100, true, "limited"}} {
		for _, dynamic := range []CoordinatorMode{{Mode: "deny"}, {Mode: "unlimited"}, {Mode: "limited", Rate: 50}, {Mode: "limited", Rate: 200}} {
			t.Run(fmt.Sprintf("%s-%s-%d", static.name, dynamic.Mode, dynamic.Rate), func(t *testing.T) {
				rate, allowed := coordinatorIntersection(static.rate, static.allowed, dynamic)
				expectedAllowed := static.allowed && dynamic.Mode != "deny"
				expectedRate := static.rate
				if dynamic.Mode == "limited" && (expectedRate == 0 || dynamic.Rate < expectedRate) {
					expectedRate = dynamic.Rate
				}
				if !expectedAllowed {
					expectedRate = 0
				}
				if allowed != expectedAllowed || rate != expectedRate {
					t.Fatalf("got %d/%v want %d/%v", rate, allowed, expectedRate, expectedAllowed)
				}
			})
		}
	}
}

// Given either finite layer rejecting a packet, neither layer is charged and
// both attempted counters retain the denied demand. Parent aggregation is per
// packet, not per child account, and networks remain separate.
func TestCoordinatorFiniteRejectionAndIsolation(t *testing.T) {
	r, now, a := coordinatorRegistry(t)
	b, other := userID("network", "b"), userID("other", "a")
	cfg := parseAccountConfig(t, `{"default":{"upload_bytes_per_second":1,"download_bytes_per_second":1,"accounts":{"default":{"upload_bytes_per_second":1,"download_bytes_per_second":1}}},"tailnets":{}}`)
	r.apply(cfg)
	r.admitAccount(b)
	r.admitAccount(other)
	r.buckets[a.network].upload.tokens = 100
	r.accounts[a].upload.tokens = 0
	r.accounts[b].upload.tokens = 100
	if r.allowAccount(a, 5, true) {
		t.Fatal("empty child allowed")
	}
	if r.buckets[a.network].upload.tokens != 100 {
		t.Fatal("child rejection charged parent")
	}
	if !r.allowAccount(b, 100, true) {
		t.Fatal("competing account rejected")
	}
	r.accounts[a].upload.tokens = 10
	if r.allowAccount(a, 5, true) || r.accounts[a].upload.tokens != 10 {
		t.Fatal("parent rejection charged child")
	}
	if !r.allowAccount(other, 9, false) {
		t.Fatal("separate network direction denied")
	}
	*now = now.Add(30 * time.Second)
	entries, _, err := r.coordinatorSnapshot()
	if err != nil || len(entries) != 5 {
		t.Fatalf("snapshot %d %v", len(entries), err)
	}
	for _, entry := range entries {
		if entry.Network == "network" && entry.Kind == "network" && (entry.Upload.Attempted != 110 || entry.Upload.Admitted != 100) {
			t.Fatalf("parent counted per child: %+v", entry)
		}
		if entry.Kind == "account" && entry.Account == a.key && entry.Network == a.network && (entry.Upload.Attempted != 10 || entry.Upload.Admitted != 0) {
			t.Fatalf("child denial lost: %+v", entry)
		}
	}
}

func TestCoordinatorBalanceFreezeAndResume(t *testing.T) {
	r, now, id := coordinatorRegistry(t)
	applyCoordinator(t, r, allocationJSON("", `{"mode":"limited","rate":10}`, `{"mode":"unlimited"}`))
	if r.allowAccount(id, 1, true) {
		t.Fatal("first finite overlay gifted burst")
	}
	*now = now.Add(time.Second)
	if !r.allowAccount(id, 6, true) {
		t.Fatal("refill unavailable")
	}
	applyCoordinator(t, r, allocationJSON("", `{"mode":"deny"}`, `{"mode":"unlimited"}`))
	*now = now.Add(10 * time.Second)
	applyCoordinator(t, r, allocationJSON("", `{"mode":"limited","rate":10}`, `{"mode":"unlimited"}`))
	if r.buckets[id.network].upload.tokens != 4 {
		t.Fatal("deny accrued tokens")
	}
	r.admitAccount(id)
	if r.allowAccount(id, 5, true) || !r.allowAccount(id, 4, true) {
		t.Fatal("reconnect refreshed frozen balance")
	}
	r.apply(parseAccountConfig(t, `{"tailnets":{}}`))
	applyCoordinator(t, r, allocationJSON("", `{"mode":"unlimited"}`, `{"mode":"unlimited"}`))
	if r.allowAccount(id, 1, true) {
		t.Fatal("overlay unlimited restored removed static policy")
	}
}

func TestCoordinatorBoundedWindows(t *testing.T) {
	for _, count := range []int{64, 65} {
		t.Run(fmt.Sprintf("parents-%d", count), func(t *testing.T) {
			r, now, _ := coordinatorRegistry(t)
			r.apply(parseAccountConfig(t, `{"default":{"upload_bytes_per_second":0,"download_bytes_per_second":0},"tailnets":{}}`))
			for i := 0; i < count; i++ {
				id := userID(fmt.Sprintf("net-%d", i), "a")
				r.admitAccount(id)
				r.allowAccount(id, 1, true)
			}
			*now = now.Add(30 * time.Second)
			entries, _, err := r.coordinatorSnapshot()
			if count == 64 && (err != nil || len(entries) != 128) {
				t.Fatalf("legal capacity: %d %v", len(entries), err)
			}
			if count == 65 && (err == nil || len(entries) != 0) {
				t.Fatal("parent overflow truncated")
			}
		})
	}
	for _, elapsed := range []time.Duration{0, time.Millisecond, 120 * time.Second, 120*time.Second + time.Nanosecond} {
		t.Run(elapsed.String(), func(t *testing.T) {
			r, now, id := coordinatorRegistry(t)
			r.allowAccount(id, 1, true)
			*now = now.Add(elapsed)
			_, window, err := r.coordinatorSnapshot()
			valid := elapsed >= time.Millisecond && elapsed <= 120*time.Second
			if (err == nil) != valid || valid && window != elapsed.Milliseconds() {
				t.Fatalf("window %d error %v", window, err)
			}
		})
	}
	r, now, id := coordinatorRegistry(t)
	if !r.allowAccount(id, int(coordinatorMaxInteger), true) {
		t.Fatal("unlimited should allow")
	}
	if !r.allowAccount(id, 1, true) {
		t.Fatal("counter overflow must not change data plane")
	}
	*now = now.Add(30 * time.Second)
	if entries, _, err := r.coordinatorSnapshot(); err == nil || len(entries) != 0 {
		t.Fatal("unsafe integer window reported")
	}
}

func TestCoordinatorDisabledHasNoCounters(t *testing.T) {
	r, _, id := coordinatorRegistry(t)
	r.coordinator = nil
	r.allowAccount(id, 5, true)
	if r.accounts[id].coordinatorActive || r.accounts[id].upload.coordinator.attempted != 0 {
		t.Fatal("disabled stats active")
	}
	if _, _, err := r.coordinatorSnapshot(); err == nil {
		t.Fatal("disabled snapshot succeeded")
	}
}

func TestCoordinatorStrictWireBoundaries(t *testing.T) {
	base := string(responseJSON(allocationJSON("", `{"mode":"limited","rate":2147483647}`, `{"mode":"deny"}`)))
	valid := []string{
		base,
		strings.Replace(base, `"generation":1`, `"generation":9007199254740991`, 1),
		strings.Replace(base, `"version":1`, fmt.Sprintf(`"vers%cu0069on":1`, 92), 1),
		strings.Replace(string(responseJSON(allocationJSON("User😀@test", `{"mode":"unlimited"}`, `{"mode":"deny"}`))), "😀", fmt.Sprintf("%cud83d%cude00", 92, 92), 1),
		strings.Replace(string(responseJSON(allocationJSON("User�@test", `{"mode":"unlimited"}`, `{"mode":"deny"}`))), "�", fmt.Sprintf("%cufffd", 92), 1),
		string(responseJSON(allocationJSON("User�@test", `{"mode":"unlimited"}`, `{"mode":"deny"}`))),
		string(responseJSON(allocationJSON("User😀@test", `{"mode":"unlimited"}`, `{"mode":"deny"}`))),
	}
	for i, data := range valid {
		if _, err := parseCoordinatorResponse([]byte(data)); err != nil {
			t.Fatalf("valid %d: %v", i, err)
		}
	}
	bad := []string{
		strings.Replace(base, `"version":1`, fmt.Sprintf(`"version":1,"vers%cu0069on":1`, 92), 1),
		strings.Replace(base, `"mode":"limited"`, fmt.Sprintf(`"mode":"limited","m%cu006fde":"deny"`, 92), 1),
		strings.Replace(base, `"rate":2147483647`, `"rate":-0`, 1),
		strings.Replace(base, `"rate":2147483647`, `"rate":1e2`, 1),
		strings.Replace(base, `"rate":2147483647`, `"rate":1.0`, 1),
		strings.Replace(base, `"generation":1`, `"generation":null`, 1),
		strings.Replace(base, `"generation":1`, `"generation":0`, 1),
		strings.Replace(base, `"generation":1`, `"generation":-1`, 1),
		strings.Replace(base, `"allocations":[`, `"allocations":null,"unknown":[`, 1),
		strings.Replace(base, `"network":"network"`, `"network":"\ud800"`, 1),
		string(responseJSON(allocationJSON("bad\n@test", `{"mode":"deny"}`, `{"mode":"deny"}`))),
		string(responseJSON(allocationJSON(" user@test", `{"mode":"deny"}`, `{"mode":"deny"}`))),
	}
	account := string(responseJSON(allocationJSON("a@test", `{"mode":"deny"}`, `{"mode":"deny"}`)))
	bad = append(bad, strings.Replace(account, `"a@test"`, `"\udfff"`, 1), strings.Replace(account, `"a@test"`, `"\ud800\ud800"`, 1), strings.Replace(account, `"a@test"`, `"`+strings.Repeat("x", 1025)+`"`, 1))
	for i, data := range bad {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			if _, err := parseCoordinatorResponse([]byte(data)); err == nil {
				t.Fatal("invalid accepted")
			}
		})
	}
	invalidUTF8 := []byte(account)
	invalidUTF8[strings.Index(account, "a@test")] = 0xff
	if _, err := parseCoordinatorResponse(invalidUTF8); err == nil {
		t.Fatal("invalid UTF8 replaced")
	}
	for _, kind := range []string{"network", "account"} {
		entries := make([]string, 0, 321)
		count := 65
		if kind == "account" {
			count = 257
		}
		for i := 0; i < count; i++ {
			entry := allocationJSON("", `{"mode":"deny"}`, `{"mode":"deny"}`)
			if kind == "account" {
				entry = allocationJSON(fmt.Sprintf("%d@test", i), `{"mode":"deny"}`, `{"mode":"deny"}`)
			} else {
				entry = strings.Replace(entry, `"network":"network"`, fmt.Sprintf(`"network":"net-%d"`, i), 1)
			}
			entries = append(entries, entry)
		}
		if _, err := parseCoordinatorResponse(responseJSON(entries...)); err == nil {
			t.Fatal("allocation capacity exceeded")
		}
	}
}

func TestCoordinatorServerAPIReloadRace(t *testing.T) {
	r, _, id := coordinatorRegistry(t)
	r.now = time.Now
	server := &Server{tailnetRate: r, tailnetAccountIdentity: true}
	path := filepath.Join(t.TempDir(), "rates.json")
	if err := os.WriteFile(path, []byte(accountConfig("100", accountLeaf(50, 50))), 0600); err != nil {
		t.Fatal(err)
	}
	body := responseJSON(allocationJSON("", `{"mode":"limited","rate":40}`, `{"mode":"unlimited"}`))
	var wg sync.WaitGroup
	for worker := 0; worker < 4; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				switch worker {
				case 0:
					r.allowAccount(id, 1, true)
				case 1:
					if err := server.LoadAndApplyTailnetRateConfig(path); err != nil {
						t.Error(err)
					}
				case 2:
					if _, err := server.ApplyTailnetCoordinatorResponse(body, "node", "boot", 1); err != nil {
						t.Error(err)
					}
				case 3:
					server.TailnetCoordinatorSnapshot()
				}
			}
		}(worker)
	}
	wg.Wait()
	if r.buckets[id.network].upload.rate != 40 {
		t.Fatal("real reload API cleared overlay")
	}
	if err := server.EnableTailnetCoordinator(); err != nil {
		t.Fatal(err)
	}
	if r.buckets[id.network].upload.burst != derp.MaxPacketSize {
		t.Fatal("burst changed")
	}
	for _, invalid := range []*Server{{}, {tailnetRate: r}} {
		if err := invalid.EnableTailnetCoordinator(); err == nil {
			t.Fatal("wrong capability enabled")
		}
		if _, _, err := invalid.TailnetCoordinatorSnapshot(); err == nil {
			t.Fatal("wrong mode snapshot")
		}
		if _, err := invalid.ApplyTailnetCoordinatorResponse(body, "node", "boot", 1); err == nil {
			t.Fatal("wrong mode response")
		}
	}
	before := r.buckets[id.network].upload.mode
	if _, err := server.ApplyTailnetCoordinatorResponse([]byte(`{}`), "node", "boot", 1); err == nil {
		t.Fatal("malformed body accepted")
	}
	if r.buckets[id.network].upload.mode != before {
		t.Fatal("malformed body applied")
	}
	entries, _, _ := server.TailnetCoordinatorSnapshot()
	if _, err := json.Marshal(entries); err != nil {
		t.Fatal(err)
	}
}
