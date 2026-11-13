// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestTailnetStartupHelperProcess(t *testing.T) {
	if os.Getenv("DERPER_TAILNET_STARTUP_HELPER") != "1" {
		return
	}
	main()
	t.Fatal("invalid startup returned instead of exiting")
}

func TestTailnetStartupBeforeSideEffects(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"URL", nil, "requires --verify-client-url"},
		{"fail open default", []string{"-verify-client-url=http://127.0.0.1/verify"}, "requires --verify-client-url-fail-open=false"},
		{"local", []string{"-verify-client-url=http://127.0.0.1/verify", "-verify-clients=true"}, "requires --verify-clients=false"},
		{"legacy", []string{"-rate-config=unused"}, "mutually exclusive"},
		{"user", []string{"-user-rate-config=unused"}, "mutually exclusive"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config := filepath.Join(t.TempDir(), "must-not-create.key")
			args := []string{"-test.run=^TestTailnetStartupHelperProcess$", "-tailnet-rate-config=unused", "-c=" + config}
			command := exec.Command(os.Args[0], append(args, tc.args...)...)
			command.Env = append(os.Environ(), "DERPER_TAILNET_STARTUP_HELPER=1")
			output, err := command.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 1 || !strings.Contains(string(output), tc.want) {
				t.Fatalf("exit=%v output=%s", err, output)
			}
			if _, err := os.Stat(config); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("startup touched server key")
			}
		})
	}
}

func TestTailnetRateStartupModes(t *testing.T) {
	for _, tc := range []struct {
		name, tailnet, user, legacy, url string
		local, failOpen, valid           bool
	}{
		{name: "disabled preserves defaults", failOpen: true, valid: true},
		{name: "legacy combinations unchanged", user: "u", legacy: "r", local: false, failOpen: true, valid: true},
		{name: "identity", tailnet: "t", url: "http://127.0.0.1/verify", valid: true},
		{name: "user exclusive", tailnet: "t", user: "u", url: "http://127.0.0.1/verify"},
		{name: "legacy exclusive", tailnet: "t", legacy: "r", url: "http://127.0.0.1/verify"},
		{name: "requires URL", tailnet: "t"},
		{name: "no local WhoIs", tailnet: "t", url: "http://127.0.0.1/verify", local: true},
		{name: "no fail open default", tailnet: "t", url: "http://127.0.0.1/verify", failOpen: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateTailnetRateMode(tc.tailnet, tc.user, tc.legacy, tc.url, tc.local, tc.failOpen)
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v err=%v", tc.valid, err)
			}
		})
	}
}
