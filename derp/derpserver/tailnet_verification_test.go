// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

package derpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

	"tailscale.com/derp"
	"tailscale.com/types/key"
)

func TestTailnetCallbackCompatibility(t *testing.T) {
	node := key.NewNode().Public()
	for _, tc := range []struct {
		name, body string
		status     int
		allow      bool
	}{
		{"identity", fmt.Sprintf(`{"Allow":true,"IdentityVersion":1,"Identity":{"NodePublic":%q,"RateLimitSubject":"network"}}`, node.String()), 200, true},
		{"legacy only", `{"Allow":true}`, 200, false},
		{"deny", `{"Allow":false}`, 200, false},
		{"empty", "", 200, false},
		{"HTTP failure", `{"Allow":true}`, 500, false},
		{"HTTP unauthorized", `{"Allow":false}`, 401, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			callback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				raw, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
				}
				fields, err := strictJSONObject(raw)
				if err != nil {
					t.Error(err)
				}
				if string(fields["IdentityVersion"]) != "1" || len(fields) != 3 || r.Header.Get("Content-Type") != "application/json" {
					t.Error("invalid v1 request", string(raw))
				}
				var echo key.NodePublic
				if err := json.Unmarshal(fields["NodePublic"], &echo); err != nil || echo != node {
					t.Error("invalid node echo")
				}
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			}))
			defer callback.Close()
			s := New(key.NewNode(), t.Logf)
			defer s.Close()
			s.SetVerifyClientURL(callback.URL)
			s.SetVerifyClientURLFailOpen(false)
			s.tailnetRate = newTailnetRateRegistry(tailnetTestConfig(t, 1, 1, derp.MaxPacketSize), s.clock.Now)
			result, err := s.verifyClient(context.Background(), node, &derp.ClientInfo{}, netip.MustParseAddr("127.0.0.1"))
			if (err == nil) != tc.allow {
				t.Fatalf("allow=%v result=%v err=%v", tc.allow, result, err)
			}
			if tc.allow && (result.tailnet != "network" || result.user != (userRateSubject{})) {
				t.Fatal("identities mixed")
			}
		})
	}
}

func TestTailnetCallbackFailsClosed(t *testing.T) {
	callback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("unsafe configuration reached callback") }))
	url := callback.URL
	callback.Close()
	for _, tc := range []struct {
		name, url       string
		local, failOpen bool
	}{
		{name: "unreachable", url: url},
		{name: "absent"},
		{name: "local WhoIs forbidden", url: url, local: true},
		{name: "fail open forbidden", url: url, failOpen: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := New(key.NewNode(), t.Logf)
			defer s.Close()
			s.SetVerifyClientURL(tc.url)
			s.SetVerifyClient(tc.local)
			s.SetVerifyClientURLFailOpen(tc.failOpen)
			s.tailnetRate = newTailnetRateRegistry(tailnetTestConfig(t, 1, 1, derp.MaxPacketSize), s.clock.Now)
			if _, err := s.verifyClient(context.Background(), key.NewNode().Public(), nil, netip.MustParseAddr("127.0.0.1")); err == nil {
				t.Fatal("unsafe admission allowed")
			}
			if len(s.tailnetRate.buckets) != 0 {
				t.Fatal("untrusted subject registered")
			}
		})
	}
}

func TestTailnetLegacyDecoderUnchanged(t *testing.T) {
	for _, body := range []string{`{"Allow":true}`, `{"allow":true}`, `{"Allow":false,"Allow":true}`, `{"Allow":true} trailing`, `{"Allow":true,"IdentityVersion":1.0}`} {
		t.Run(body, func(t *testing.T) {
			callback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				data, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
				}
				if strings.Contains(string(data), "IdentityVersion") {
					t.Error("legacy request changed")
				}
				fmt.Fprint(w, body)
			}))
			defer callback.Close()
			s := New(key.NewNode(), t.Logf)
			defer s.Close()
			s.SetVerifyClientURL(callback.URL)
			result, err := s.verifyClient(context.Background(), key.NewNode().Public(), nil, netip.MustParseAddr("127.0.0.1"))
			if err != nil || result != (admissionResult{}) {
				t.Fatalf("legacy behavior changed: %v %v", result, err)
			}
		})
	}
}
