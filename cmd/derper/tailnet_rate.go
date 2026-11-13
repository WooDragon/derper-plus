// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"context"
	"errors"
	"log"
	"os"
	"os/signal"
	"syscall"

	"tailscale.com/derp/derpserver"
)

// Validate before starting listeners, loading keys, or creating background work.
// The legacy modes keep their original validation and semantics.
func validateTailnetRateMode(tailnet, user, legacy, url string, local, failOpen bool) error {
	if tailnet == "" {
		return nil
	}
	if user != "" || legacy != "" {
		return errors.New("--tailnet-rate-config, --user-rate-config and --rate-config are mutually exclusive")
	}
	if url == "" {
		return errors.New("--tailnet-rate-config requires --verify-client-url")
	}
	if local {
		return errors.New("--tailnet-rate-config requires --verify-clients=false")
	}
	if failOpen {
		return errors.New("--tailnet-rate-config requires --verify-client-url-fail-open=false")
	}
	return nil
}

func watchTailnetRateConfig(ctx context.Context, s *derpserver.Server, path string) {
	sighup := make(chan os.Signal, 1)
	signal.Notify(sighup, syscall.SIGHUP)
	defer signal.Stop(sighup)
	for {
		select {
		case <-ctx.Done():
			return
		case <-sighup:
			if err := s.LoadAndApplyTailnetRateConfig(path); err != nil {
				log.Printf("derper: tailnet rate config reload failed: %v", err)
				continue
			}
			log.Printf("derper: tailnet rate config reloaded successfully")
		}
	}
}
