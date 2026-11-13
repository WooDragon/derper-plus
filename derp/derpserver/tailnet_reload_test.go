// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

package derpserver

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"tailscale.com/derp"
	"tailscale.com/types/key"
)

func TestTailnetOverrideAndDirections(t *testing.T) {
	cfg, err := parseTailnetRateConfig([]byte(`{"default":{"upload_bytes_per_second":1,"download_bytes_per_second":1},"tailnets":{"upload-free":{"upload_bytes_per_second":0,"download_bytes_per_second":1},"download-free":{"upload_bytes_per_second":1,"download_bytes_per_second":0},"all-free":{"upload_bytes_per_second":0,"download_bytes_per_second":0}}}`))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(100, 0)
	reg := newTailnetRateRegistry(cfg, func() time.Time { return now })
	for _, tc := range []struct {
		subject          string
		upFree, downFree bool
	}{
		{"default-a", false, false}, {"default-b", false, false}, {"upload-free", true, false}, {"download-free", false, true}, {"all-free", true, true},
	} {
		t.Run(tc.subject, func(t *testing.T) {
			if !reg.admit(tc.subject) {
				t.Fatal("not admitted")
			}
			for _, direction := range []struct{ upload, free bool }{{true, tc.upFree}, {false, tc.downFree}} {
				if !reg.allow(tc.subject, derp.MaxPacketSize, direction.upload) {
					t.Fatal("initial burst")
				}
				if got := reg.allow(tc.subject, 1, direction.upload); got != direction.free {
					t.Fatalf("upload=%v second byte=%v", direction.upload, got)
				}
			}
		})
	}
	// Removing a complete override selects default, with no fresh finite bucket.
	withoutOverrides := tailnetTestConfig(t, 1, 1, derp.MaxPacketSize)
	reg.apply(withoutOverrides)
	if reg.allow("all-free", 1, true) || reg.allow("all-free", 1, false) {
		t.Fatal("override deletion minted tokens")
	}
	reg.apply(tailnetRateConfig{})
	if reg.admit("new") || reg.allow("default-a", 0, true) {
		t.Fatal("missing policy was not deny")
	}
}

func TestTailnetInvalidReloadKeepsEntireConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rates.json")
	s := New(key.NewNode(), t.Logf)
	defer s.Close()
	write := func(data string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(tailnetTCPConfig(1, 0))
	if err := s.LoadAndApplyTailnetRateConfig(path); err != nil {
		t.Fatal(err)
	}
	registry := s.tailnetRate
	registry.admit("network")
	registry.allow("network", derp.MaxPacketSize, true)
	for _, invalid := range []string{"", `{"tailnets":{},"default":{"upload_bytes_per_second":0}}`, strings.Repeat(" ", maxTailnetRateConfigSize+1)} {
		write(invalid)
		if err := s.LoadAndApplyTailnetRateConfig(path); err == nil {
			t.Fatal("bad reload accepted")
		}
		if s.tailnetRate != registry || registry.allow("network", derp.MaxPacketSize, true) || !registry.allow("network", derp.MaxPacketSize, false) {
			t.Fatal("invalid reload changed existing policy or balance")
		}
	}
	if err := s.LoadAndApplyTailnetRateConfig(path + ".missing"); err == nil {
		t.Fatal("missing config accepted")
	}
}

func TestTailnetFractionalTokens(t *testing.T) {
	now := time.Unix(100, 0)
	reg := newTailnetRateRegistry(tailnetTestConfig(t, 1, 1, derp.MaxPacketSize), func() time.Time { return now })
	reg.admit("a")
	reg.allow("a", derp.MaxPacketSize, true)
	now = now.Add(400 * time.Millisecond)
	if reg.allow("a", 1, true) {
		t.Fatal("fraction rounded up")
	}
	reg.apply(tailnetTestConfig(t, 1, 1, derp.MaxPacketSize))
	now = now.Add(600 * time.Millisecond)
	if !reg.allow("a", 1, true) || reg.allow("a", 1, true) {
		t.Fatal("fraction lost or manufactured")
	}
}

func TestTailnetRegistryConcurrentReload(t *testing.T) {
	cfg := tailnetTestConfig(t, 1, 1, derp.MaxPacketSize)
	reg := newTailnetRateRegistry(cfg, time.Now)
	var wg sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for attempt := 0; attempt < 100; attempt++ {
				subject := fmt.Sprint("network-", attempt%10)
				reg.admit(subject)
				reg.allow(subject, 1, true)
				reg.allow(subject, 1, false)
				reg.apply(cfg)
			}
		}()
	}
	wg.Wait()
	if len(reg.buckets) != 10 {
		t.Fatal("lost registered subject")
	}
}

func TestTailnetParserAdditionalBoundaries(t *testing.T) {
	for _, input := range []string{
		`null`, `{"tailnets":[]}`, `{"tailnets":{"a":null}}`,
		`{"tailnets":{"a":{"upload_bytes_per_second":0,"download_bytes_per_second":0,"extra":0}}}`,
		`{"tailnets":{},"default":{"Upload_bytes_per_second":0,"download_bytes_per_second":0}}`,
		`{"tailnets":{},"default":{"upload_bytes_per_second":0,"upload_bytes_per_second":1,"download_bytes_per_second":0}}`,
		`{"tailnets":{"a":{"upload_bytes_per_second":0,"download_bytes_per_second":0},"a":{"upload_bytes_per_second":0,"download_bytes_per_second":0}}}`,
		`{"tailnets":{},"default":{"upload_bytes_per_second":0,"download_bytes_per_second":0,"burst_bytes":65536,"burst_bytes":65537}}`,
	} {
		t.Run(input, func(t *testing.T) {
			if _, err := parseTailnetRateConfig([]byte(input)); err == nil {
				t.Fatal("invalid config accepted")
			}
		})
	}
	for _, subject := range []string{"", strings.Repeat("a", 65), "a.b", "a b", "é", "a/", "\n"} {
		if validRateLimitSubject(subject) {
			t.Fatalf("invalid subject %q", subject)
		}
	}
	if !validRateLimitSubject(strings.Repeat("Z", 64)) || !validRateLimitSubject("Az09_-z") {
		t.Fatal("valid subject rejected")
	}
}
