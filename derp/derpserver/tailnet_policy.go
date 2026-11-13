// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

package derpserver

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"

	"tailscale.com/derp"
)

const maxTailnetRateValue = 1<<31 - 1
const maxTailnetRateConfigSize = 1 << 20

type tailnetRatePolicy struct {
	upload, download, burst int
}

type tailnetRateConfig struct {
	defaultPolicy *tailnetRatePolicy
	policies      map[string]tailnetRatePolicy
}

func validRateLimitSubject(subject string) bool {
	if len(subject) < 1 || len(subject) > 64 {
		return false
	}
	for _, c := range []byte(subject) {
		if c != '_' && c != '-' && !(c >= 'a' && c <= 'z') && !(c >= 'A' && c <= 'Z') && !(c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}

// strictJSONObject preserves presence and number tokens and rejects decoded
// duplicate names (including escaped aliases). It never folds field case.
func strictJSONObject(data []byte) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return nil, errors.New("expected JSON object")
	}
	fields := make(map[string]json.RawMessage)
	for decoder.More() {
		token, err = decoder.Token()
		if err != nil {
			return nil, err
		}
		name, ok := token.(string)
		if !ok {
			return nil, errors.New("expected field name")
		}
		if _, ok := fields[name]; ok {
			return nil, fmt.Errorf("duplicate field %q", name)
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, err
		}
		fields[name] = value
	}
	if _, err := decoder.Token(); err != nil {
		return nil, err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, errors.New("trailing JSON content")
	}
	return fields, nil
}

func parseTailnetRateConfig(data []byte) (tailnetRateConfig, error) {
	cfg := tailnetRateConfig{policies: make(map[string]tailnetRatePolicy)}
	if len(data) > maxTailnetRateConfigSize {
		return cfg, errors.New("tailnet rate config exceeds 1 MiB")
	}
	fields, err := strictJSONObject(data)
	if err != nil {
		return cfg, err
	}
	for name := range fields {
		if name != "default" && name != "tailnets" {
			return cfg, fmt.Errorf("unknown config field %q", name)
		}
	}
	networks, err := strictJSONObject(fields["tailnets"])
	if err != nil {
		return cfg, fmt.Errorf("tailnets: %w", err)
	}
	if raw, ok := fields["default"]; ok {
		policy, err := parseTailnetPolicy(raw)
		if err != nil {
			return cfg, fmt.Errorf("default: %w", err)
		}
		cfg.defaultPolicy = &policy
	}
	for subject, raw := range networks {
		if !validRateLimitSubject(subject) {
			return cfg, errors.New("invalid tailnet policy subject")
		}
		policy, err := parseTailnetPolicy(raw)
		if err != nil {
			return cfg, fmt.Errorf("tailnet policy: %w", err)
		}
		cfg.policies[subject] = policy
	}
	return cfg, nil
}

func parseTailnetPolicy(data []byte) (tailnetRatePolicy, error) {
	policy := tailnetRatePolicy{burst: derp.MaxPacketSize}
	fields, err := strictJSONObject(data)
	if err != nil {
		return policy, err
	}
	for name := range fields {
		if name != "upload_bytes_per_second" && name != "download_bytes_per_second" && name != "burst_bytes" {
			return policy, fmt.Errorf("unknown policy field %q", name)
		}
	}
	policy.upload, err = parseTailnetRateNumber(fields["upload_bytes_per_second"])
	if err != nil {
		return policy, fmt.Errorf("upload: %w", err)
	}
	policy.download, err = parseTailnetRateNumber(fields["download_bytes_per_second"])
	if err != nil {
		return policy, fmt.Errorf("download: %w", err)
	}
	if raw, ok := fields["burst_bytes"]; ok {
		policy.burst, err = parseTailnetRateNumber(raw)
		if err != nil {
			return policy, fmt.Errorf("burst: %w", err)
		}
		if policy.burst < derp.MaxPacketSize {
			return policy, errors.New("burst below maximum packet size")
		}
	}
	return policy, nil
}

func parseTailnetRateNumber(raw []byte) (int, error) {
	text := string(bytes.TrimSpace(raw))
	n, err := strconv.ParseUint(text, 10, 31)
	if err != nil || strconv.FormatUint(n, 10) != text {
		return 0, errors.New("expected integer in [0, 2147483647]")
	}
	return int(n), nil
}

func (cfg tailnetRateConfig) policyFor(subject string) (tailnetRatePolicy, bool) {
	if policy, ok := cfg.policies[subject]; ok {
		return policy, true
	}
	if cfg.defaultPolicy != nil {
		return *cfg.defaultPolicy, true
	}
	return tailnetRatePolicy{}, false
}

// LoadAndApplyTailnetRateConfig validates outside the registry lock. It must
// first be called before Accept; later calls atomically reload the same mode.
func (s *Server) LoadAndApplyTailnetRateConfig(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxTailnetRateConfigSize+1))
	if err != nil {
		return err
	}
	cfg, err := parseTailnetRateConfig(data)
	if err != nil {
		return err
	}
	if s.tailnetRate == nil {
		s.tailnetRate = newTailnetRateRegistry(cfg, s.clock.Now)
	} else {
		s.tailnetRate.apply(cfg)
	}
	return nil
}
