// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

package derpserver

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"tailscale.com/types/key"
)

func TestAccountIdentityRegistryBoundary(t *testing.T) {
	r := newTailnetRateRegistry(parseAccountConfig(t, accountConfig("0", accountLeaf(0, 0))), func() time.Time { return time.Unix(1, 0) })
	for _, id := range []tailnetAccountIdentity{
		{}, userID("bad.network", "a"), userID(strings.Repeat("n", 65), "a"),
		accountID("network", "user", ""), accountID("network", "user", " leading"),
		accountID("network", "user", strings.Repeat("雪", 342)), accountID("network", "user", "a\nb"),
		accountID("network", "tagged", "other"), accountID("network", "unknown", "shared"),
	} {
		if r.admitAccount(id) || r.allowAccount(id, 0, true) {
			t.Fatal("untrusted identity entered registry")
		}
	}
	if len(r.accounts) != 0 || len(r.buckets) != 0 {
		t.Fatal("invalid identity created history")
	}
	id := userID("network", "a")
	if r.allowAccount(id, 0, true) || len(r.accounts) != 0 {
		t.Fatal("packet path lazily registered identity")
	}
	if !r.admitAccount(id) || !r.allowAccount(id, 0, true) {
		t.Fatal("trusted unlimited identity not admitted")
	}
	// Negative sizes never turn into a credit, including when both layers are 0.
	if r.allowAccount(id, -1, true) {
		t.Fatal("negative size credited")
	}
}

// Given an admitted identity, packet lookup must not confer its registration
// on a different account, kind, or network, even when that parent exists.
func TestAccountPacketRegistrationBoundary(t *testing.T) {
	for _, upload := range []bool{true, false} {
		t.Run(fmt.Sprint(upload), func(t *testing.T) {
			r := newTailnetRateRegistry(parseAccountConfig(t, accountConfig("1", accountLeaf(1, 1))), func() time.Time { return time.Unix(1, 0) })
			id := userID("network", "a")
			if !r.admitAccount(id) {
				t.Fatal("trusted identity not admitted")
			}
			for _, candidate := range []tailnetAccountIdentity{
				userID("network", "b"), userID("other", "a"),
				accountID("network", "tagged", "shared"),
				accountID("network", "user", "shared"),
				accountID("network", "user", strings.ToUpper(id.key)),
				accountID("network", "unknown", id.key), {},
			} {
				if r.allowAccount(candidate, 0, upload) || r.allowAccount(candidate, 1, upload) {
					t.Fatal("packet lookup bypassed exact identity registration")
				}
			}
			if r.allowAccount(id, -1, upload) {
				t.Fatal("negative packet admitted")
			}
			if len(r.accounts) != 1 || len(r.buckets) != 1 {
				t.Fatal("packet lookup created history")
			}
			if !r.allowAccount(id, 1, upload) || directionalBucket(r.accounts[id], upload).tokens != float64(r.config.policies[id.network].burst-1) || directionalBucket(r.buckets[id.network], upload).tokens != float64(r.config.policies[id.network].burst-1) {
				t.Fatal("rejected lookup changed a registered balance")
			}
			if !r.allowAccount(id, 1, !upload) {
				t.Fatal("packet lookup disturbed other direction")
			}
			// Defensive lookup also requires the parent, even if a child exists.
			delete(r.buckets, id.network)
			if r.allowAccount(id, 0, upload) || r.allowAccount(id, 1, upload) {
				t.Fatal("child lookup bypassed missing parent")
			}
			if directionalBucket(r.accounts[id], upload).tokens != float64(r.config.policies[id.network].burst-1) {
				t.Fatal("missing parent debited child")
			}
		})
	}
}

func TestAccountV1Compatibility(t *testing.T) {
	node := key.NewNode().Public()
	// v1 historically ignored unknown fields, including an Account object; v2's
	// full whitelist must not be retrofitted onto that decoder.
	raw := fmt.Sprintf(`{"Allow":true,"IdentityVersion":1,"Extra":true,"Identity":{"NodePublic":%q,"RateLimitSubject":"network","Account":{"Kind":"unknown"},"Extra":1}}`, node.String())
	if subject, err := parseTailnetAdmission(strings.NewReader(raw), node); err != nil || subject != "network" {
		t.Fatal("v1 semantics tightened", err)
	}
	if _, err := parseAccountAdmission(strings.NewReader(raw), node); err == nil {
		t.Fatal("v2 downgraded")
	}
}

func TestAccountEnabledCapabilityFrozen(t *testing.T) {
	a := key.NewNode()
	h := newAccountTCPHarness(t, tailnetTCPConfig(0, 0), map[key.NodePublic]tailnetAccountIdentity{a.Public(): userID("network", "a")}, 2)
	if h.server.SetTailnetAccountIdentity(false) == nil || h.server.SetTailnetAccountIdentity(true) == nil {
		t.Fatal("enabled capability mutable after registry")
	}
	h.connect(t, a, true)
	if h.server.SetTailnetAccountIdentity(false) == nil {
		t.Fatal("enabled capability mutable after Accept")
	}
	h.reload(t, accountTCPConfig(0, 1, "upload"))
	if !h.server.tailnetAccountIdentity {
		t.Fatal("reload changed capability")
	}
}

func TestAccountSameNowSettlement(t *testing.T) {
	now := time.Unix(1, 0)
	calls := 0
	r := newTailnetRateRegistry(parseAccountConfig(t, accountConfig("1", accountLeaf(1, 1))), func() time.Time { calls++; return now })
	id := userID("network", "a")
	r.admitAccount(id)
	if calls != 1 {
		t.Fatal("admit did not use one instant")
	}
	if !r.allowAccount(id, 1, true) {
		t.Fatal("debit")
	}
	now = now.Add(time.Second)
	calls = 0
	// Both empty to start, then both settle by exactly one token on rejection.
	pb, cb := &r.buckets[id.network].upload, &r.accounts[id].upload
	pb.tokens = 0
	cb.tokens = 0
	if r.allowAccount(id, 2, true) {
		t.Fatal("insufficient debit admitted")
	}
	if calls != 1 || pb.tokens != 1 || cb.tokens != 1 || !pb.last.Equal(cb.last) {
		t.Fatal("rejection did not settle both at same now")
	}
	calls = 0
	r.apply(r.config)
	if calls != 1 {
		t.Fatal("reload did not use one instant")
	}
}
