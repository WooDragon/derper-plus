// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

package derpserver

import (
	"errors"
	"fmt"
)

// tailnetAccountIdentity is trusted callback evidence, never a client label.
// Keep the network in the key: opaque user keys alone are not bucket identities.
type tailnetAccountIdentity struct {
	network, kind, key string
}

func validAccountKey(key string) bool {
	if len(key) != 67 || key[:3] != "h1_" {
		return false
	}
	for _, c := range key[3:] {
		if !(c >= '0' && c <= '9') && !(c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func (id tailnetAccountIdentity) valid() bool {
	return validRateLimitSubject(id.network) && ((id.kind == "user" && validAccountKey(id.key)) || (id.kind == "tagged" && id.key == "shared"))
}

type tailnetAccountPolicies struct {
	defaultPolicy, tagged tailnetRatePolicy
	overrides             map[string]tailnetRatePolicy
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
		if name != "default" && name != "tagged" && name != "overrides" {
			return nil, errors.New("unknown accounts field")
		}
	}
	policies := &tailnetAccountPolicies{overrides: make(map[string]tailnetRatePolicy)}
	policies.defaultPolicy, err = parseTailnetLeafPolicy(fields["default"])
	if err != nil {
		return nil, fmt.Errorf("accounts.default: %w", err)
	}
	policies.tagged, err = parseTailnetLeafPolicy(fields["tagged"])
	if err != nil {
		return nil, fmt.Errorf("accounts.tagged: %w", err)
	}
	if raw, ok := fields["overrides"]; ok {
		overrides, err := strictJSONObject(raw)
		if err != nil {
			return nil, fmt.Errorf("accounts.overrides: %w", err)
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
	if id.kind == "tagged" {
		return accounts.tagged, true
	}
	if policy, ok := accounts.overrides[id.key]; ok {
		return policy, true
	}
	return accounts.defaultPolicy, true
}
