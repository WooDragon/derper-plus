// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"tailscale.com/derp/derpserver"
)

var tailnetCoordinatorConfigPath = flag.String("tailnet-coordinator-config", "", "path to startup-only HTTPS coordinator config (endpoint, nodeId, tokenFile); requires tailnet rate config and account identity")
var coordinatorProcessStartedAt = time.Now().UnixMilli()

const coordinatorInterval = 30 * time.Second
const coordinatorTimeout = 5 * time.Second
const coordinatorMaxSeq uint64 = 1<<53 - 1

type coordinatorSource interface {
	TailnetCoordinatorSnapshot() ([]derpserver.CoordinatorEntry, int64, error)
	ApplyTailnetCoordinatorResponse([]byte, string, string, uint64) (uint64, error)
}
type coordinatorClient struct {
	config          derpserver.CoordinatorConfig
	token, bootID   string
	bootStartedAtMS int64
	seq             uint64
	http            *http.Client
}

func validateTailnetCoordinatorMode(config, tailnet string, account bool) error {
	if config != "" && (tailnet == "" || !account) {
		return errors.New("--tailnet-coordinator-config requires --tailnet-rate-config and --tailnet-account-identity")
	}
	return nil
}

func validateCoordinatorEndpoint(endpoint string, allowLoopback bool) error {
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return errors.New("invalid coordinator endpoint")
	}
	loopback := net.ParseIP(parsed.Hostname())
	testHTTP := allowLoopback && parsed.Scheme == "http" && loopback != nil && loopback.IsLoopback()
	if (parsed.Scheme != "https" && !testHTTP) || parsed.Hostname() == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || parsed.Path != "/coordinator/report" || parsed.RawPath != "" || parsed.Opaque != "" {
		return errors.New("coordinator endpoint must be an HTTPS /coordinator/report URL without credentials, query or fragment")
	}
	return nil
}

func newCoordinatorClient(config derpserver.CoordinatorConfig, token string, allowLoopback bool) (*coordinatorClient, error) {
	if err := validateCoordinatorEndpoint(config.Endpoint, allowLoopback); err != nil {
		return nil, err
	}
	// A Bearer credential is a single ASCII token. Never include it in errors.
	if token == "" {
		return nil, errors.New("empty coordinator token")
	}
	for _, c := range []byte(token) {
		if c <= 32 || c >= 127 {
			return nil, errors.New("invalid coordinator token")
		}
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return nil, errors.New("coordinator boot entropy unavailable")
	}
	return &coordinatorClient{
		config: config, token: token, bootID: hex.EncodeToString(random[:]), bootStartedAtMS: coordinatorProcessStartedAt,
		http: &http.Client{Timeout: coordinatorTimeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
	}, nil
}

func readCoordinatorFile(path string, limit int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, errors.New("coordinator file unavailable")
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil || int64(len(data)) > limit {
		return nil, errors.New("invalid coordinator file size or read")
	}
	return data, nil
}

func loadCoordinatorClient(path string) (*coordinatorClient, error) {
	data, err := readCoordinatorFile(path, derpserver.CoordinatorMaxBody)
	if err != nil {
		return nil, err
	}
	config, err := derpserver.ParseCoordinatorConfig(data)
	if err != nil {
		return nil, err
	}
	token, err := readCoordinatorFile(config.TokenFile, 4096)
	if err != nil {
		return nil, err
	}
	// Permit one conventional trailing newline, not arbitrary whitespace trimming.
	value := strings.TrimSuffix(strings.TrimSuffix(string(token), "\n"), "\r")
	return newCoordinatorClient(config, value, false)
}

func (client *coordinatorClient) report(ctx context.Context, source coordinatorSource) error {
	entries, window, err := source.TailnetCoordinatorSnapshot()
	if err != nil {
		return errors.New("window_invalid")
	}
	if client.seq == coordinatorMaxSeq {
		return errors.New("sequence_exhausted")
	}
	client.seq++
	report := derpserver.CoordinatorReport{Version: 1, NodeID: client.config.NodeID, BootID: client.bootID, BootStartedAtMS: client.bootStartedAtMS, Seq: client.seq, WindowMS: window, Entries: entries}
	body, err := json.Marshal(report)
	if err != nil || len(body) > derpserver.CoordinatorMaxBody {
		return errors.New("report_invalid")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, client.config.Endpoint, bytes.NewReader(body))
	if err != nil {
		return errors.New("request_invalid")
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+client.token)
	response, err := client.http.Do(request)
	if err != nil {
		return errors.New("request_failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return errors.New("http_status")
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, derpserver.CoordinatorMaxBody+1))
	if err != nil || len(raw) > derpserver.CoordinatorMaxBody {
		return errors.New("response_body")
	}
	revision, err := source.ApplyTailnetCoordinatorResponse(raw, client.config.NodeID, client.bootID, client.seq)
	if err != nil {
		return errors.New("response_invalid")
	}
	log.Printf("derper: coordinator node=%s entries=%d policy_revision=%d success", client.config.NodeID, len(entries), revision)
	return nil
}

// A single loop owns seq and the HTTP request. Production uses a time.Ticker
// (which drops ticks while busy); tests inject ticks without a production flag.
func (client *coordinatorClient) run(ctx context.Context, source coordinatorSource, ticks <-chan time.Time) {
	var failures uint64
	for {
		if ctx.Err() != nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case _, ok := <-ticks:
			if !ok || ctx.Err() != nil {
				return
			}
			if err := client.report(ctx, source); err != nil {
				failures++
				log.Printf("derper: coordinator node=%s failure=%s failures=%d", client.config.NodeID, err, failures)
			}
		}
	}
}

func startTailnetCoordinator(ctx context.Context, server *derpserver.Server) error {
	if *tailnetCoordinatorConfigPath == "" {
		return nil
	}
	client, err := loadCoordinatorClient(*tailnetCoordinatorConfigPath)
	if err != nil {
		return err
	}
	if err := server.EnableTailnetCoordinator(); err != nil {
		return err
	}
	go func() {
		ticker := time.NewTicker(coordinatorInterval)
		defer ticker.Stop()
		client.run(ctx, server, ticker.C)
	}()
	return nil
}
