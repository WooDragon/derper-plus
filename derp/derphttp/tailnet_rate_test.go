// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

package derphttp_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"tailscale.com/derp"
	"tailscale.com/derp/derphttp"
	"tailscale.com/derp/derpserver"
	"tailscale.com/net/netmon"
	"tailscale.com/types/key"
)

// Exercise the HTTP Upgrade path, real client private-key handshake, callback,
// and policy admission together. Connect alone does not establish admission.
func TestTailnetHTTPAdmission(t *testing.T) {
	for _, tc := range []struct {
		name                                string
		policy, identity, echo, deny, allow bool
	}{
		{name: "v1", policy: true, identity: true, echo: true, allow: true},
		{name: "legacy allow is not identity", policy: true},
		{name: "wrong echo", policy: true, identity: true},
		{name: "deny precedence", policy: true, identity: true, echo: true, deny: true},
		{name: "missing policy", identity: true, echo: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			private := key.NewNode()
			callback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req struct {
					NodePublic      key.NodePublic
					IdentityVersion int
				}
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Error(err)
				}
				if req.NodePublic != private.Public() || req.IdentityVersion != 1 {
					t.Error("wrong identity request")
				}
				echo := private.Public()
				if !tc.echo {
					echo = key.NewNode().Public()
				}
				if !tc.identity {
					fmt.Fprint(w, `{"Allow":true}`)
					return
				}
				fmt.Fprintf(w, `{"Allow":%t,"IdentityVersion":1,"Identity":{"NodePublic":%q,"RateLimitSubject":"network-0"}}`, !tc.deny, echo.String())
			}))
			defer callback.Close()
			server := derpserver.New(key.NewNode(), t.Logf)
			defer server.Close()
			server.SetVerifyClientURL(callback.URL)
			server.SetVerifyClientURLFailOpen(false)
			path := filepath.Join(t.TempDir(), "rates.json")
			policy := `{"tailnets":{}}`
			if tc.policy {
				policy = `{"tailnets":{},"default":{"upload_bytes_per_second":0,"download_bytes_per_second":0}}`
			}
			if err := os.WriteFile(path, []byte(policy), 0600); err != nil {
				t.Fatal(err)
			}
			if err := server.LoadAndApplyTailnetRateConfig(path); err != nil {
				t.Fatal(err)
			}
			endpoint := httptest.NewServer(derpserver.Handler(server))
			defer endpoint.Close()
			client, err := derphttp.NewClient(private, endpoint.URL, t.Logf, netmon.NewStatic())
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if err := client.Connect(ctx); err != nil {
				t.Fatal(err)
			}
			verifyTailnetHTTPAdmission(t, client, tc.allow)
		})
	}
}

func verifyTailnetHTTPAdmission(t *testing.T, client *derphttp.Client, allow bool) {
	t.Helper()
	type result struct {
		message derp.ReceivedMessage
		err     error
	}
	done := make(chan result, 1)
	go func() { message, err := client.Recv(); done <- result{message, err} }()
	select {
	case got := <-done:
		if !allow {
			if got.err == nil {
				t.Fatalf("denied client received %T", got.message)
			}
			return
		}
		if got.err != nil {
			t.Fatal(got.err)
		}
		if _, ok := got.message.(derp.ServerInfoMessage); !ok {
			t.Fatalf("first message was %T", got.message)
		}
	case <-time.After(3 * time.Second):
		client.Close()
		<-done
		t.Fatal("admission result timed out")
	}
}
