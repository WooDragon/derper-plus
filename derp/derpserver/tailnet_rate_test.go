// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

package derpserver

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"tailscale.com/derp"
)

// Given complete policies, each subject selects an override, an explicit default,
// or deny. No node or reconnect may mint another initial burst.
func TestTailnetPolicyParsing(t *testing.T) {
	policy := `{"upload_bytes_per_second":1,"download_bytes_per_second":0}`
	for _, tc := range []struct {
		name, input string
		valid       bool
	}{
		{"empty networks", `{"tailnets":{}}`, true},
		{"default", `{"tailnets":{},"default":` + policy + `}`, true},
		{"override", `{"tailnets":{"Network_0":` + policy + `}}`, true},
		{"missing networks", `{}`, false},
		{"null networks", `{"tailnets":null}`, false},
		{"null default", `{"tailnets":{},"default":null}`, false},
		{"partial override", `{"tailnets":{"a":{"upload_bytes_per_second":1}}}`, false},
		{"bad subject", `{"tailnets":{"a.b":` + policy + `}}`, false},
		{"unknown", `{"tailnets":{},"other":0}`, false},
		{"duplicate", `{"tailnets":{},"tailnets":{}}`, false},
		{"escaped duplicate", fmt.Sprintf(`{"tailnets":{},"%cu0074ailnets":{}}`, 92), false},
		{"trailing", `{"tailnets":{}} {}`, false},
		{"array", `[]`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseTailnetRateConfig([]byte(tc.input))
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v err=%v", tc.valid, err)
			}
		})
	}
	for _, field := range []string{"upload_bytes_per_second", "download_bytes_per_second", "burst_bytes"} {
		for _, value := range []string{"null", `"1"`, "-1", "1.0", "1e0", "2147483648", "18446744073709551616", "true"} {
			t.Run(field+"/"+value, func(t *testing.T) {
				input := fmt.Sprintf(`{"tailnets":{},"default":{"upload_bytes_per_second":1,"download_bytes_per_second":1,"burst_bytes":%d}}`, derp.MaxPacketSize)
				old := `"` + field + `":1`
				if field == "burst_bytes" {
					old = fmt.Sprintf(`"burst_bytes":%d`, derp.MaxPacketSize)
				}
				input = strings.Replace(input, old, `"`+field+`":`+value, 1)
				if _, err := parseTailnetRateConfig([]byte(input)); err == nil {
					t.Fatal("invalid numeric value accepted")
				}
			})
		}
	}
	for _, burst := range []int{0, derp.MaxPacketSize - 1, derp.MaxPacketSize, 2147483647} {
		t.Run(fmt.Sprint("burst/", burst), func(t *testing.T) {
			input := fmt.Sprintf(`{"tailnets":{},"default":{"upload_bytes_per_second":2147483647,"download_bytes_per_second":0,"burst_bytes":%d}}`, burst)
			_, err := parseTailnetRateConfig([]byte(input))
			if (err == nil) != (burst >= derp.MaxPacketSize) {
				t.Fatalf("burst=%d err=%v", burst, err)
			}
		})
	}
}

func tailnetTestConfig(t *testing.T, upload, download, burst int) tailnetRateConfig {
	t.Helper()
	cfg, err := parseTailnetRateConfig([]byte(fmt.Sprintf(`{"tailnets":{},"default":{"upload_bytes_per_second":%d,"download_bytes_per_second":%d,"burst_bytes":%d}}`, upload, download, burst)))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestTailnetIndependentSubjects(t *testing.T) {
	now := time.Unix(100, 0)
	reg := newTailnetRateRegistry(tailnetTestConfig(t, 1, 1, derp.MaxPacketSize), func() time.Time { return now })
	for n := 0; n < 10; n++ {
		subject := fmt.Sprint("network-", n)
		if !reg.admit(subject) {
			t.Fatal(subject)
		}
		if !reg.allow(subject, derp.MaxPacketSize, true) {
			t.Fatal("independent upload", subject)
		}
		if reg.allow(subject, 1, true) {
			t.Fatal("upload minted tokens")
		}
		if !reg.allow(subject, derp.MaxPacketSize, false) {
			t.Fatal("independent download")
		}
		if !reg.admit(subject) || reg.allow(subject, 1, false) {
			t.Fatal("reconnect minted tokens")
		}
	}
}

func TestTailnetLifecycle(t *testing.T) {
	const burst = derp.MaxPacketSize
	now := time.Unix(100, 0)
	clock := func() time.Time { return now }
	finite := tailnetTestConfig(t, 100, 200, burst)
	unlimited := tailnetTestConfig(t, 0, 0, burst)
	deny := tailnetRateConfig{policies: map[string]tailnetRatePolicy{}}
	for _, initial := range []struct {
		name     string
		cfg      tailnetRateConfig
		admitted bool
	}{
		{"limited", finite, true}, {"unlimited", unlimited, true}, {"deny", deny, false},
	} {
		t.Run(initial.name, func(t *testing.T) {
			reg := newTailnetRateRegistry(initial.cfg, clock)
			if got := reg.admit("a"); got != initial.admitted {
				t.Fatalf("admit=%v", got)
			}
			reg.apply(finite)
			want := 0
			if initial.name == "limited" {
				want = burst
			}
			if !reg.allow("a", want, true) || reg.allow("a", 1, true) {
				t.Fatal("first finite balance")
			}
			now = now.Add(500 * time.Millisecond)
			reg.apply(finite)
			if !reg.allow("a", 50, true) || reg.allow("a", 1, true) {
				t.Fatal("same policy or fractional refill")
			}
			now = now.Add(250 * time.Millisecond)
			reg.apply(unlimited) // freeze 25 tokens under old rate
			now = now.Add(24 * time.Hour)
			if !reg.allow("a", burst, true) {
				t.Fatal("unlimited")
			}
			reg.apply(deny)
			if reg.allow("a", 0, true) || reg.admit("a") {
				t.Fatal("deny")
			}
			now = now.Add(time.Hour)
			reg.apply(finite)
			if !reg.allow("a", 25, true) || reg.allow("a", 1, true) {
				t.Fatal("inactive time refilled balance")
			}
		})
	}
}

func TestTailnetBurstSettlement(t *testing.T) {
	now := time.Unix(100, 0)
	reg := newTailnetRateRegistry(tailnetTestConfig(t, 100, 0, 2*derp.MaxPacketSize), func() time.Time { return now })
	reg.admit("a")
	reg.allow("a", derp.MaxPacketSize, true)
	now = now.Add(time.Second)
	reg.apply(tailnetTestConfig(t, 1000, 0, derp.MaxPacketSize))
	reg.apply(tailnetTestConfig(t, 1000, 0, 2*derp.MaxPacketSize))
	if !reg.allow("a", derp.MaxPacketSize, true) || reg.allow("a", 1, true) {
		t.Fatal("shrink/grow manufactured tokens")
	}
	now = now.Add(500 * time.Millisecond)
	reg.apply(tailnetTestConfig(t, 1, 0, 2*derp.MaxPacketSize))
	if !reg.allow("a", 500, true) || reg.allow("a", 1, true) {
		t.Fatal("did not settle old rate")
	}
	now = now.Add(time.Duration(1<<63 - 1))
	if !reg.allow("a", 2*derp.MaxPacketSize, true) {
		t.Fatal("long duration did not refill")
	}
}
