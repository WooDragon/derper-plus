package derpserver

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func coordinatorRegistry(t *testing.T) (*tailnetRateRegistry, *time.Time, tailnetAccountIdentity) {
	t.Helper()
	now := time.Unix(1, 0)
	r := newTailnetRateRegistry(parseAccountConfig(t, accountConfig("0", accountLeaf(0, 0))), func() time.Time { return now })
	id := userID("network", "a")
	r.admitAccount(id)
	r.enableCoordinator()
	return r, &now, id
}
func allocationJSON(account string, up, down string) string {
	subject := `"kind":"network","network":"network"`
	if account != "" {
		subject = `"kind":"account","network":"network","account":` + fmt.Sprintf("%q", account)
	}
	return `{` + subject + `,"upload":` + up + `,"download":` + down + `}`
}
func responseJSON(allocations ...string) []byte {
	return []byte(`{"version":1,"nodeId":"node","bootId":"boot","acceptedSeq":1,"generation":1,"policyRevision":1,"allocations":[` + strings.Join(allocations, ",") + `]}`)
}
func applyCoordinator(t *testing.T, r *tailnetRateRegistry, allocations ...string) {
	t.Helper()
	response, err := parseCoordinatorResponse(responseJSON(allocations...))
	if err != nil {
		t.Fatal(err)
	}
	if err := r.applyCoordinator(response, "node", "boot", 1); err != nil {
		t.Fatal(err)
	}
}

// Given a denied parent or child, both attempted counters advance exactly once,
// while neither admitted counter advances. A new window forgets idle history.
func TestCoordinatorCountersAndWindows(t *testing.T) {
	r, now, id := coordinatorRegistry(t)
	if !r.allowAccount(id, 10, true) {
		t.Fatal("static unlimited denied")
	}
	applyCoordinator(t, r, allocationJSON(id.key, `{"mode":"deny"}`, `{"mode":"unlimited"}`))
	if r.allowAccount(id, 20, true) {
		t.Fatal("dynamic zero share became unlimited")
	}
	applyCoordinator(t, r, allocationJSON("", `{"mode":"deny"}`, `{"mode":"unlimited"}`))
	if r.allowAccount(id, 30, true) {
		t.Fatal("parent deny bypassed")
	}
	if !r.allowAccount(id, 7, false) {
		t.Fatal("direction isolation")
	}
	*now = now.Add(30 * time.Second)
	entries, window, err := r.coordinatorSnapshot()
	if err != nil || window != 30000 || len(entries) != 2 {
		t.Fatalf("snapshot: %v %d %+v", err, window, entries)
	}
	for _, entry := range entries {
		if entry.Upload.Attempted != 60 || entry.Upload.Admitted != 10 || entry.Download.Attempted != 7 || entry.Download.Admitted != 7 {
			t.Fatalf("wrong counters: %+v", entry)
		}
	}
	*now = now.Add(30 * time.Second)
	entries, _, err = r.coordinatorSnapshot()
	if err != nil || len(entries) != 0 {
		t.Fatalf("idle historical bucket reported: %+v %v", entries, err)
	}
}

func TestCoordinatorIntersectionAndNoBurst(t *testing.T) {
	r, now, id := coordinatorRegistry(t)
	r.apply(parseAccountConfig(t, accountConfig("100", accountLeaf(80, 80))))
	*now = now.Add(time.Second)
	applyCoordinator(t, r, allocationJSON("", `{"mode":"limited","rate":50}`, `{"mode":"unlimited"}`), allocationJSON(id.key, `{"mode":"limited","rate":200}`, `{"mode":"unlimited"}`))
	if r.buckets[id.network].upload.rate != 50 || r.accounts[id].upload.rate != 80 || r.buckets[id.network].download.rate != 100 {
		t.Fatal("not static intersection")
	}
	r.buckets[id.network].upload.tokens = 0
	applyCoordinator(t, r, allocationJSON("", `{"mode":"limited","rate":50}`, `{"mode":"unlimited"}`))
	if r.allowAccount(id, 1, true) {
		t.Fatal("same response refilled burst")
	}
	r.apply(parseAccountConfig(t, accountConfig("200", accountLeaf(20, 20))))
	if r.buckets[id.network].upload.rate != 50 || r.accounts[id].upload.rate != 20 {
		t.Fatal("reload lost overlay")
	}
	applyCoordinator(t, r)
	if r.buckets[id.network].upload.rate != 50 {
		t.Fatal("missing allocation cleared overlay")
	}
	fresh := newTailnetRateRegistry(r.config, func() time.Time { return *now })
	fresh.admitAccount(id)
	if fresh.buckets[id.network].upload.rate != 200 {
		t.Fatal("cold boot retained overlay")
	}
	newID := userID("network", "new")
	r.admitAccount(newID)
	if r.accounts[newID].upload.rate != 20 {
		t.Fatal("new identity did not use static fallback")
	}
}

func TestCoordinatorAtomicResponse(t *testing.T) {
	for _, mutation := range []string{"unknown", "node", "boot", "seq", "generation"} {
		t.Run(mutation, func(t *testing.T) {
			r, _, id := coordinatorRegistry(t)
			applyCoordinator(t, r, allocationJSON("", `{"mode":"limited","rate":50}`, `{"mode":"unlimited"}`))
			data := responseJSON(allocationJSON("", `{"mode":"deny"}`, `{"mode":"deny"}`), allocationJSON(id.key, `{"mode":"deny"}`, `{"mode":"deny"}`))
			switch mutation {
			case "unknown":
				data = []byte(strings.ReplaceAll(string(data), id.key, "unknown@synthetic.test"))
			case "node":
				data = []byte(strings.Replace(string(data), `"nodeId":"node"`, `"nodeId":"other"`, 1))
			case "boot":
				data = []byte(strings.Replace(string(data), `"bootId":"boot"`, `"bootId":"other"`, 1))
			case "seq":
				data = []byte(strings.Replace(string(data), `"acceptedSeq":1`, `"acceptedSeq":2`, 1))
			case "generation":
				data = []byte(strings.Replace(string(data), `"generation":1`, `"generation":0`, 1))
			}
			response, err := parseCoordinatorResponse(data)
			if err == nil {
				err = r.applyCoordinator(response, "node", "boot", 1)
			}
			if err == nil {
				t.Fatal("invalid response accepted")
			}
			if r.buckets[id.network].upload.mode != tailnetLimited {
				t.Fatal("partial update")
			}
		})
	}
}

func TestCoordinatorStrictWire(t *testing.T) {
	base := string(responseJSON(allocationJSON("User😀@test", `{"mode":"limited","rate":1}`, `{"mode":"unlimited"}`)))
	if _, err := parseCoordinatorResponse([]byte(base)); err != nil {
		t.Fatal(err)
	}
	bad := []string{
		strings.Replace(base, `"version":1`, `"version":1,"version":1`, 1),
		strings.Replace(base, `"version":1`, `"version":1e0`, 1),
		strings.Replace(base, `"version":1`, `"version":1.0`, 1),
		strings.Replace(base, `"generation":1`, `"generation":-0`, 1),
		strings.Replace(base, `"generation":1`, `"generation":9007199254740992`, 1),
		strings.Replace(base, `"rate":1`, `"rate":0`, 1),
		strings.Replace(base, `"rate":1`, `"rate":2147483648`, 1),
		strings.Replace(base, `"mode":"unlimited"`, `"mode":"unlimited","rate":1`, 1),
		strings.Replace(base, `"account":"User😀@test"`, `"account":"\ud800@test"`, 1),
		strings.Replace(base, `"kind":"account"`, `"kind":"network"`, 1),
		strings.Replace(base, `"policyRevision":1,`, ``, 1),
		strings.Replace(base, `"version":1`, `"Version":1`, 1),
		strings.Replace(base, `"mode":"limited"`, `"mode":"limited","unknown":0`, 1),
		base + `{}`,
	}
	for i, input := range bad {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			if _, err := parseCoordinatorResponse([]byte(input)); err == nil {
				t.Fatal("invalid wire accepted")
			}
		})
	}
	for _, user := range []string{`"😀@test"`, `"�@test"`, `"�@test"`} {
		data := strings.Replace(base, `"User😀@test"`, user, 1)
		if _, err := parseCoordinatorResponse([]byte(data)); err != nil {
			t.Fatalf("valid unicode %s: %v", user, err)
		}
	}
	if _, err := parseCoordinatorResponse(make([]byte, (1<<20)+1)); err == nil {
		t.Fatal("oversize response accepted")
	}
	duplicate := responseJSON(allocationJSON("", `{"mode":"deny"}`, `{"mode":"deny"}`), allocationJSON("", `{"mode":"deny"}`, `{"mode":"deny"}`))
	if _, err := parseCoordinatorResponse(duplicate); err == nil {
		t.Fatal("duplicate subject accepted")
	}
}

func TestCoordinatorCapacityAndPause(t *testing.T) {
	r, now, id := coordinatorRegistry(t)
	for i := 0; i < 257; i++ {
		account := userID("network", fmt.Sprint(i))
		r.admitAccount(account)
		r.allowAccount(account, 1, true)
	}
	*now = now.Add(30 * time.Second)
	if _, _, err := r.coordinatorSnapshot(); err == nil {
		t.Fatal("capacity truncated into complete report")
	}
	r.allowAccount(id, 1, true)
	*now = now.Add(121 * time.Second)
	if _, _, err := r.coordinatorSnapshot(); err == nil {
		t.Fatal("long pause reported as valid window")
	}
	r.allowAccount(id, 2, true)
	*now = now.Add(30 * time.Second)
	entries, _, err := r.coordinatorSnapshot()
	if err != nil || len(entries) != 2 || entries[0].Upload.Attempted != 2 {
		t.Fatalf("window did not reset: %+v %v", entries, err)
	}
	jsonData, err := json.Marshal(entries)
	if err != nil || !strings.Contains(string(jsonData), `"effective":{"mode":"unlimited"}`) {
		t.Fatalf("wire shape: %s %v", jsonData, err)
	}
}

// Given concurrent packets, reloads and complete responses, all three operations
// share one linearization lock. Run this scenario under the race detector.
func TestCoordinatorConcurrentReload(t *testing.T) {
	r, _, id := coordinatorRegistry(t)
	cfg := r.config
	response, err := parseCoordinatorResponse(responseJSON(allocationJSON("", `{"mode":"limited","rate":50}`, `{"mode":"unlimited"}`)))
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for worker := 0; worker < 3; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				switch worker {
				case 0:
					r.allowAccount(id, 1, true)
				case 1:
					r.apply(cfg)
				case 2:
					if err := r.applyCoordinator(response, "node", "boot", 1); err != nil {
						t.Error(err)
					}
				}
			}
		}(worker)
	}
	wg.Wait()
	if r.buckets[id.network].upload.rate != 50 {
		t.Fatal("overlay lost to reload")
	}
}
