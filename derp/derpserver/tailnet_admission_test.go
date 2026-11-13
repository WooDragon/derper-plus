// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

package derpserver

import (
	"fmt"
	"strings"
	"testing"

	"tailscale.com/types/key"
)

func TestTailnetAdmissionWire(t *testing.T) {
	node := key.NewNode().Public()
	identity := fmt.Sprintf(`{"NodePublic":%q,"RateLimitSubject":"network-0"}`, node.String())
	good := `{"Allow":true,"IdentityVersion":1,"Identity":` + identity + `}`
	for _, tc := range []struct {
		name, body string
		allow      bool
	}{
		{"v1", good, true},
		{"surrounding whitespace", " \n" + good + "\t", true},
		{"legacy", `{"Allow":true}`, false},
		{"denied", `{"Allow":false}`, false},
		{"deny precedence", strings.Replace(good, `true`, `false`, 1), false},
		{"missing allow", `{"IdentityVersion":1,"Identity":` + identity + `}`, false},
		{"null allow", strings.Replace(good, `true`, `null`, 1), false},
		{"string allow", strings.Replace(good, `true`, `"true"`, 1), false},
		{"case alias", strings.Replace(good, `"Allow"`, `"allow"`, 1), false},
		{"null identity", strings.Replace(good, identity, `null`, 1), false},
		{"missing identity", `{"Allow":true,"IdentityVersion":1}`, false},
		{"wrong echo", strings.Replace(good, node.String(), key.NewNode().Public().String(), 1), false},
		{"malformed key", strings.Replace(good, node.String(), "nodekey:bad", 1), false},
		{"noncanonical key", strings.Replace(good, node.String(), strings.ToUpper(node.String()), 1), false},
		{"duplicate allow", strings.Replace(good, `"Allow":true`, `"Allow":false,"Allow":true`, 1), false},
		{"escaped duplicate allow", strings.Replace(good, `"Allow":true`, fmt.Sprintf(`"Allow":false,"%cu0041llow":true`, 92), 1), false},
		{"escaped duplicate version", strings.Replace(good, `"IdentityVersion":1`, fmt.Sprintf(`"IdentityVersion":0,"%cu0049dentityVersion":1`, 92), 1), false},
		{"escaped duplicate subject", strings.Replace(good, `"RateLimitSubject":`, fmt.Sprintf(`"RateLimitSubject":"other","%cu0052ateLimitSubject":`, 92), 1), false},
		{"duplicate version", strings.Replace(good, `"IdentityVersion":1`, `"IdentityVersion":0,"IdentityVersion":1`, 1), false},
		{"duplicate identity", strings.Replace(good, `"Identity":`, `"Identity":{},"Identity":`, 1), false},
		{"duplicate node", strings.Replace(good, `"NodePublic":`, fmt.Sprintf(`"NodePublic":%q,"NodePublic":`, node.String()), 1), false},
		{"duplicate subject", strings.Replace(good, `"RateLimitSubject":`, `"RateLimitSubject":"other","RateLimitSubject":`, 1), false},
		{"trailing", good + ` {}`, false},
		{"junk", good + ` x`, false},
		{"truncated", good[:len(good)-1], false},
		{"4096", good + strings.Repeat(" ", 4096-len(good)), true},
		{"4097", good + strings.Repeat(" ", 4097-len(good)), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			subject, err := parseTailnetAdmission(strings.NewReader(tc.body), node)
			if (err == nil) != tc.allow {
				t.Fatalf("subject=%q err=%v", subject, err)
			}
			if tc.allow && subject != "network-0" {
				t.Fatal(subject)
			}
		})
	}
	for _, version := range []string{"0", "2", "1.0", "1e0", "1.0000000000000001", "null", `"1"`, "true", "{}", "[]"} {
		t.Run("version/"+version, func(t *testing.T) {
			body := strings.Replace(good, `"IdentityVersion":1`, `"IdentityVersion":`+version, 1)
			if _, err := parseTailnetAdmission(strings.NewReader(body), node); err == nil {
				t.Fatal("bad version accepted")
			}
		})
	}
	for _, subject := range []string{"", " ", "a.b", "a/b", "网络", strings.Repeat("a", 65)} {
		t.Run("subject/"+subject, func(t *testing.T) {
			body := strings.Replace(good, `"network-0"`, fmt.Sprintf("%q", subject), 1)
			if _, err := parseTailnetAdmission(strings.NewReader(body), node); err == nil {
				t.Fatal("bad subject accepted")
			}
		})
	}
}
