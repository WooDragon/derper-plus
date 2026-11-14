package derpserver

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestCoordinatorRejectsGenerationRollback(t *testing.T) {
	r, _, id := coordinatorRegistry(t)
	body := responseJSON(allocationJSON("", `{"mode":"limited","rate":50}`, `{"mode":"unlimited"}`))
	newer := []byte(strings.Replace(string(body), `"generation":1`, `"generation":2`, 1))
	response, err := parseCoordinatorResponse(newer)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.applyCoordinator(response, "node", "boot", 1); err != nil {
		t.Fatal(err)
	}
	response, err = parseCoordinatorResponse(responseJSON(allocationJSON("", `{"mode":"deny"}`, `{"mode":"deny"}`)))
	if err != nil {
		t.Fatal(err)
	}
	if err := r.applyCoordinator(response, "node", "boot", 1); err == nil {
		t.Fatal("older positive generation accepted")
	}
	if r.buckets[id.network].upload.rate != 50 || r.coordinator.generation != 2 {
		t.Fatal("rollback mutated overlay")
	}
	// The static reload path retains the overlay even after policy removal and return.
	r.apply(parseAccountConfig(t, `{"tailnets":{}}`))
	if r.buckets[id.network].upload.mode != tailnetDeny {
		t.Fatal("removed static policy not denied")
	}
	r.apply(parseAccountConfig(t, accountConfig("0", accountLeaf(0, 0))))
	if r.buckets[id.network].upload.rate != 50 {
		t.Fatal("overlay lost across static deny")
	}
}

func TestCoordinatorPacketBoundaryAndMaximumCapacity(t *testing.T) {
	r, now, id := coordinatorRegistry(t)
	if r.allowAccount(userID("network", "unknown"), 1, true) || r.allowAccount(id, -1, true) {
		t.Fatal("invalid packet boundary")
	}
	if !r.allowAccount(id, 0, true) {
		t.Fatal("zero payload changed data semantics")
	}
	*now = now.Add(30 * time.Second)
	if entries, _, err := r.coordinatorSnapshot(); err != nil || len(entries) != 0 {
		t.Fatal("invalid packets created activity")
	}
	for i := 0; i < 256; i++ {
		account := userID("network", fmt.Sprint(i))
		r.admitAccount(account)
		r.allowAccount(account, 1, true)
	}
	*now = now.Add(30 * time.Second)
	entries, _, err := r.coordinatorSnapshot()
	if err != nil || len(entries) != 257 {
		t.Fatalf("max account window rejected %d %v", len(entries), err)
	}
	// Snapshot reports effective modes at the atomic swap, not last report's modes.
	applyCoordinator(t, r, allocationJSON("", `{"mode":"deny"}`, `{"mode":"limited","rate":10}`))
	r.allowAccount(id, 5, true)
	*now = now.Add(30 * time.Second)
	entries, _, err = r.coordinatorSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Kind == "network" && (entry.Upload.Effective.Mode != "deny" || entry.Download.Effective.Mode != "limited" || entry.Download.Effective.Rate != 10) {
			t.Fatalf("stale effective modes: %+v", entry)
		}
	}
}
