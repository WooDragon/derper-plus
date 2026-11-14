// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

package derpserver

import (
	"bytes"
	"encoding/json"
	"errors"
	"strconv"
)

const CoordinatorMaxBody = 1 << 20
const coordinatorMaxInteger uint64 = 1<<53 - 1

// CoordinatorSubject is an exact trusted registry identity, not a client label.
type CoordinatorSubject struct {
	Kind    string `json:"kind"`
	Network string `json:"network"`
	Account string `json:"account,omitempty"`
}
type CoordinatorMode struct {
	Mode string `json:"mode"`
	Rate int    `json:"rate,omitempty"`
}
type CoordinatorDirection struct {
	Attempted uint64          `json:"attempted"`
	Admitted  uint64          `json:"admitted"`
	Effective CoordinatorMode `json:"effective"`
}
type CoordinatorEntry struct {
	CoordinatorSubject
	Upload   CoordinatorDirection `json:"upload"`
	Download CoordinatorDirection `json:"download"`
}
type CoordinatorReport struct {
	Version         int                `json:"version"`
	NodeID          string             `json:"nodeId"`
	BootID          string             `json:"bootId"`
	BootStartedAtMS int64              `json:"bootStartedAtMs"`
	Seq             uint64             `json:"seq"`
	WindowMS        int64              `json:"windowMs"`
	Entries         []CoordinatorEntry `json:"entries"`
}
type coordinatorAllocation struct {
	CoordinatorSubject
	upload, download CoordinatorMode
}
type coordinatorResponse struct {
	node, boot                string
	seq, generation, revision uint64
	allocations               []coordinatorAllocation
}

func coordinatorInteger(raw []byte, min, max uint64) (uint64, error) {
	text := string(bytes.TrimSpace(raw))
	value, err := strconv.ParseUint(text, 10, 64)
	if err != nil || value < min || value > max || strconv.FormatUint(value, 10) != text {
		return 0, errors.New("invalid coordinator integer")
	}
	return value, nil
}

func coordinatorString(raw []byte) (string, error) {
	var value string
	if !validAccountJSONUnicode(raw) || json.Unmarshal(raw, &value) != nil || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return "", errors.New("invalid coordinator string")
	}
	return value, nil
}

func parseCoordinatorMode(raw []byte) (CoordinatorMode, error) {
	var result CoordinatorMode
	fields, err := strictJSONObject(raw)
	if err != nil {
		return result, err
	}
	result.Mode, err = coordinatorString(fields["mode"])
	if err != nil {
		return result, err
	}
	switch result.Mode {
	case "deny", "unlimited":
		if len(fields) == 1 {
			return result, nil
		}
	case "limited":
		if len(fields) == 2 {
			rate, err := coordinatorInteger(fields["rate"], 1, maxTailnetRateValue)
			result.Rate = int(rate)
			return result, err
		}
	}
	return result, errors.New("invalid coordinator mode")
}

func parseCoordinatorAllocation(raw []byte) (coordinatorAllocation, error) {
	var result coordinatorAllocation
	fields, err := strictJSONObject(raw)
	if err != nil {
		return result, err
	}
	result.Kind, err = coordinatorString(fields["kind"])
	if err != nil {
		return result, err
	}
	names := []string{"kind", "network", "upload", "download"}
	if result.Kind == "account" {
		names = append(names, "account")
	}
	if _, err := exactIdentityObject(raw, names...); err != nil {
		return result, err
	}
	result.Network, err = coordinatorString(fields["network"])
	if err != nil || !validRateLimitSubject(result.Network) {
		return result, errors.New("invalid coordinator network")
	}
	switch result.Kind {
	case "network":
	case "account":
		result.Account, err = coordinatorString(fields["account"])
		if err != nil || !validAccountKey(result.Account) {
			return result, errors.New("invalid coordinator account")
		}
	default:
		return result, errors.New("invalid coordinator kind")
	}
	result.upload, err = parseCoordinatorMode(fields["upload"])
	if err != nil {
		return result, err
	}
	result.download, err = parseCoordinatorMode(fields["download"])
	return result, err
}

func validCoordinatorBootID(value string) bool {
	if len(value) == 0 || len(value) > 128 {
		return false
	}
	for _, c := range []byte(value) {
		if c != '_' && c != '-' && !(c >= 'a' && c <= 'z') && !(c >= 'A' && c <= 'Z') && !(c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}

func parseCoordinatorResponse(raw []byte) (coordinatorResponse, error) {
	var result coordinatorResponse
	if len(raw) > CoordinatorMaxBody || !validAccountJSONUnicode(raw) {
		return result, errors.New("invalid coordinator body")
	}
	fields, err := exactIdentityObject(raw, "version", "nodeId", "bootId", "acceptedSeq", "generation", "policyRevision", "allocations")
	if err != nil {
		return result, err
	}
	if _, err := coordinatorInteger(fields["version"], 1, 1); err != nil {
		return result, err
	}
	result.node, err = coordinatorString(fields["nodeId"])
	if err != nil || !validRateLimitSubject(result.node) {
		return result, errors.New("invalid coordinator node")
	}
	result.boot, err = coordinatorString(fields["bootId"])
	if err != nil || !validCoordinatorBootID(result.boot) {
		return result, errors.New("invalid coordinator boot")
	}
	for name, target := range map[string]*uint64{"acceptedSeq": &result.seq, "generation": &result.generation, "policyRevision": &result.revision} {
		*target, err = coordinatorInteger(fields[name], 1, coordinatorMaxInteger)
		if err != nil {
			return result, err
		}
	}
	result.allocations, err = parseCoordinatorAllocations(fields["allocations"])
	return result, err
}

func parseCoordinatorAllocations(raw []byte) ([]coordinatorAllocation, error) {
	var entries []json.RawMessage
	if len(bytes.TrimSpace(raw)) == 0 || bytes.TrimSpace(raw)[0] != '[' || json.Unmarshal(raw, &entries) != nil || len(entries) > 320 {
		return nil, errors.New("invalid coordinator allocations")
	}
	result := make([]coordinatorAllocation, 0, len(entries))
	seen := make(map[CoordinatorSubject]bool)
	accounts, parents := 0, 0
	for _, raw := range entries {
		entry, err := parseCoordinatorAllocation(raw)
		if err != nil {
			return nil, err
		}
		if seen[entry.CoordinatorSubject] {
			return nil, errors.New("duplicate coordinator subject")
		}
		seen[entry.CoordinatorSubject] = true
		if entry.Kind == "account" {
			accounts++
		} else {
			parents++
		}
		result = append(result, entry)
	}
	if accounts > 256 || parents > 64 {
		return nil, errors.New("coordinator capacity exceeded")
	}
	return result, nil
}

// CoordinatorConfig is startup-only; TokenFile contains the existing admission
// token. Endpoint validation and token loading belong to the HTTP client.
type CoordinatorConfig struct{ Endpoint, NodeID, TokenFile string }

func ParseCoordinatorConfig(raw []byte) (CoordinatorConfig, error) {
	var result CoordinatorConfig
	if len(raw) > CoordinatorMaxBody || !validAccountJSONUnicode(raw) {
		return result, errors.New("invalid coordinator config")
	}
	fields, err := exactIdentityObject(raw, "endpoint", "nodeId", "tokenFile")
	if err != nil {
		return result, errors.New("invalid coordinator config fields")
	}
	for name, target := range map[string]*string{"endpoint": &result.Endpoint, "nodeId": &result.NodeID, "tokenFile": &result.TokenFile} {
		*target, err = coordinatorString(fields[name])
		if err != nil || *target == "" {
			return result, errors.New("invalid coordinator config value")
		}
	}
	if !validRateLimitSubject(result.NodeID) {
		return result, errors.New("invalid coordinator nodeId")
	}
	return result, nil
}
