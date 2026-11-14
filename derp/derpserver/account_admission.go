// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

package derpserver

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"unicode/utf8"

	"tailscale.com/types/key"
)

func exactIdentityObject(raw []byte, names ...string) (map[string]json.RawMessage, error) {
	fields, err := strictJSONObject(raw)
	if err != nil {
		return nil, err
	}
	if len(fields) != len(names) {
		return nil, errors.New("incorrect identity fields")
	}
	for _, name := range names {
		if _, ok := fields[name]; !ok {
			return nil, errors.New("missing identity field")
		}
	}
	return fields, nil
}

// encoding/json replaces malformed UTF-8 and unpaired surrogate escapes with
// U+FFFD. Reject those before decoding account values or override names; valid
// U+FFFD and escaped backslash text must retain their distinct identities.
func validAccountJSONUnicode(raw []byte) bool {
	if !utf8.Valid(raw) || !json.Valid(raw) {
		return false
	}
	for i := 0; i < len(raw); i++ {
		if raw[i] != '\\' {
			continue
		}
		i++
		if raw[i] != 'u' {
			continue
		}
		code, _ := strconv.ParseUint(string(raw[i+1:i+5]), 16, 16)
		i += 4
		if code < 0xd800 || code > 0xdfff {
			continue
		}
		if code > 0xdbff || i+6 >= len(raw) || raw[i+1] != '\\' || raw[i+2] != 'u' {
			return false
		}
		low, _ := strconv.ParseUint(string(raw[i+3:i+7]), 16, 16)
		if low < 0xdc00 || low > 0xdfff {
			return false
		}
		i += 6
	}
	return true
}

func parseAccountAdmission(reader io.Reader, node key.NodePublic) (tailnetAccountIdentity, error) {
	var id tailnetAccountIdentity
	data, err := io.ReadAll(io.LimitReader(reader, 4097))
	if err != nil {
		return id, err
	}
	if len(data) > 4096 {
		return id, errors.New("identity response exceeds 4096 bytes")
	}
	fields, err := exactIdentityObject(data, "Allow", "IdentityVersion", "Identity")
	if err != nil {
		return id, err
	}
	if !bytes.Equal(bytes.TrimSpace(fields["Allow"]), []byte("true")) || !bytes.Equal(bytes.TrimSpace(fields["IdentityVersion"]), []byte("2")) {
		return id, errors.New("identity admission requires allowing version 2")
	}
	identity, err := exactIdentityObject(fields["Identity"], "NodePublic", "RateLimitSubject", "Account")
	if err != nil {
		return id, err
	}
	var echo string
	if err := json.Unmarshal(identity["NodePublic"], &echo); err != nil {
		return id, errors.New("invalid identity echo")
	}
	if echo != node.String() {
		return id, errors.New("identity node key mismatch")
	}
	if err := json.Unmarshal(identity["RateLimitSubject"], &id.network); err != nil {
		return id, errors.New("invalid identity network")
	}
	account, err := exactIdentityObject(identity["Account"], "Kind", "Key")
	if err != nil {
		return id, err
	}
	if err := json.Unmarshal(account["Kind"], &id.kind); err != nil {
		return id, errors.New("invalid account kind")
	}
	if !validAccountJSONUnicode(account["Key"]) {
		return id, errors.New("invalid account key Unicode")
	}
	if err := json.Unmarshal(account["Key"], &id.key); err != nil {
		return id, errors.New("invalid account key")
	}
	if !id.valid() {
		return tailnetAccountIdentity{}, errors.New("invalid account identity")
	}
	return id, nil
}

// SetTailnetAccountIdentity selects a startup capability, not a reloadable
// policy. Call before loading the tailnet registry and before the first Accept.
func (s *Server) SetTailnetAccountIdentity(enabled bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tailnetRate != nil || s.accepts.Value() != 0 {
		return errors.New("tailnet account capability is frozen")
	}
	s.tailnetAccountIdentity = enabled
	return nil
}
