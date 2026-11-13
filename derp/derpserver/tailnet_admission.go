// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

package derpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/netip"
	"time"

	"tailscale.com/types/key"
)

// admissionResult keeps independent identities for the two opt-in modes. A
// trusted mesh connection carries neither identity and bypasses both policers.
type admissionResult struct {
	user    userRateSubject
	tailnet string
}

type tailnetAdmissionRequest struct {
	NodePublic      key.NodePublic
	Source          netip.Addr
	IdentityVersion int
}

func parseTailnetAdmission(reader io.Reader, node key.NodePublic) (string, error) {
	data, err := io.ReadAll(io.LimitReader(reader, 4097))
	if err != nil {
		return "", err
	}
	if len(data) > 4096 {
		return "", errors.New("identity response exceeds 4096 bytes")
	}
	fields, err := strictJSONObject(data)
	if err != nil {
		return "", err
	}
	if !bytes.Equal(bytes.TrimSpace(fields["Allow"]), []byte("true")) {
		return "", errors.New("identity admission denied")
	}
	if !bytes.Equal(bytes.TrimSpace(fields["IdentityVersion"]), []byte("1")) {
		return "", errors.New("identity response requires version 1")
	}
	identity, err := strictJSONObject(fields["Identity"])
	if err != nil {
		return "", err
	}
	var echo, subject string
	if err := json.Unmarshal(identity["NodePublic"], &echo); err != nil {
		return "", errors.New("invalid identity node key")
	}
	var echoedKey key.NodePublic
	if err := echoedKey.UnmarshalText([]byte(echo)); err != nil || echoedKey != node || echoedKey.String() != echo {
		return "", errors.New("identity node key mismatch")
	}
	if err := json.Unmarshal(identity["RateLimitSubject"], &subject); err != nil || !validRateLimitSubject(subject) {
		return "", errors.New("invalid identity subject")
	}
	return subject, nil
}

func (s *Server) verifyTailnetClient(ctx context.Context, node key.NodePublic, source netip.Addr) (admissionResult, error) {
	reject := func() (admissionResult, error) {
		return admissionResult{}, userRateAdmissionError("tailnet identity admission rejected")
	}
	// API users cannot accidentally bypass the CLI's safety constraints.
	if s.verifyClientsURL == "" || s.verifyClientsLocalTailscaled || s.verifyClientsURLFailOpen || s.userRate != nil {
		return reject()
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	data, err := json.Marshal(tailnetAdmissionRequest{NodePublic: node, Source: source, IdentityVersion: 1})
	if err != nil {
		return reject()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.verifyClientsURL, bytes.NewReader(data))
	if err != nil {
		return reject()
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return reject()
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return reject()
	}
	subject, err := parseTailnetAdmission(res.Body, node)
	if err != nil {
		return reject()
	}
	// Even a currently denied subject must keep its trusted admission history.
	if !s.tailnetRate.admit(subject) {
		return reject()
	}
	return admissionResult{tailnet: subject}, nil
}
