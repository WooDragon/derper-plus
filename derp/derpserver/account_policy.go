// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

package derpserver

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// tailnetAccountIdentity is trusted callback evidence, never a client label.
// Keep the network in the key: registering accounts alone are not bucket identities.
type tailnetAccountIdentity struct {
	network, kind, key string
}

func validAccountKey(key string) bool {
	if len(key) == 0 || len(key) > 1024 || !utf8.ValidString(key) {
		return false
	}
	if strings.TrimFunc(key, func(r rune) bool { return unicode.IsSpace(r) || r == '\uFEFF' }) != key {
		return false
	}
	for _, r := range key {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func (id tailnetAccountIdentity) valid() bool {
	return validRateLimitSubject(id.network) && id.kind == "user" && validAccountKey(id.key)
}

type tailnetAccountPolicies struct {
	defaultPolicy tailnetRatePolicy
	overrides     map[string]tailnetRatePolicy
}

func parseTailnetPolicy(data []byte) (tailnetRatePolicy, error) {
	fields, err := strictJSONObject(data)
	if err != nil {
		return tailnetRatePolicy{}, err
	}
	raw, hasAccounts := fields["accounts"]
	delete(fields, "accounts")
	policy, err := parseTailnetLeafFields(fields)
	if err != nil || !hasAccounts {
		return policy, err
	}
	policy.accounts, err = parseAccountPolicies(raw)
	return policy, err
}

func parseAccountPolicies(raw []byte) (*tailnetAccountPolicies, error) {
	fields, err := strictJSONObject(raw)
	if err != nil {
		return nil, fmt.Errorf("accounts: %w", err)
	}
	for name := range fields {
		if name != "default" && name != "overrides" {
			return nil, errors.New("unknown accounts field")
		}
	}
	policies := &tailnetAccountPolicies{overrides: make(map[string]tailnetRatePolicy)}
	policies.defaultPolicy, err = parseTailnetLeafPolicy(fields["default"])
	if err != nil {
		return nil, fmt.Errorf("accounts.default: %w", err)
	}
	if raw, ok := fields["overrides"]; ok {
		if !validAccountJSONUnicode(raw) {
			return nil, errors.New("invalid accounts.overrides Unicode")
		}
		overrides, err := strictJSONObject(raw)
		if err != nil {
			// Decoded field names are registering accounts, not safe log data.
			return nil, errors.New("invalid accounts.overrides object")
		}
		for key, raw := range overrides {
			if !validAccountKey(key) {
				return nil, errors.New("invalid account override key")
			}
			policy, err := parseTailnetLeafPolicy(raw)
			if err != nil {
				return nil, fmt.Errorf("account override: %w", err)
			}
			policies.overrides[key] = policy
		}
	}
	return policies, nil
}

func (cfg tailnetRateConfig) hasAccounts() bool {
	if cfg.defaultPolicy != nil && cfg.defaultPolicy.accounts != nil {
		return true
	}
	for _, policy := range cfg.policies {
		if policy.accounts != nil {
			return true
		}
	}
	return false
}

func (cfg tailnetRateConfig) accountPolicyFor(id tailnetAccountIdentity) (tailnetRatePolicy, bool) {
	parent, applicable := cfg.policyFor(id.network)
	if !applicable || parent.accounts == nil {
		return tailnetRatePolicy{}, applicable
	}
	accounts := parent.accounts
	if policy, ok := accounts.overrides[id.key]; ok {
		return policy, true
	}
	return accounts.defaultPolicy, true
}
