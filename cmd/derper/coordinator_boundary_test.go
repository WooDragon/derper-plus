package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"tailscale.com/derp/derpserver"
	"tailscale.com/types/key"
)

type coordinatorWindowSource struct {
	coordinatorTestSource
	entries []derpserver.CoordinatorEntry
	invalid bool
}

func (s *coordinatorWindowSource) TailnetCoordinatorSnapshot() ([]derpserver.CoordinatorEntry, int64, error) {
	if s.invalid {
		return nil, 0, errors.New("invalid window")
	}
	return s.entries, 30000, nil
}

func TestCoordinatorClientHTTPSAndEntryShape(t *testing.T) {
	entry := derpserver.CoordinatorEntry{
		CoordinatorSubject: derpserver.CoordinatorSubject{Kind: "account", Network: "network", Account: "User😀@test"},
		Upload:             derpserver.CoordinatorDirection{Attempted: 15, Admitted: 10, Effective: derpserver.CoordinatorMode{Mode: "limited", Rate: 10}},
		Download:           derpserver.CoordinatorDirection{Effective: derpserver.CoordinatorMode{Mode: "deny"}},
	}
	source := &coordinatorWindowSource{entries: []derpserver.CoordinatorEntry{entry}}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		// Compare the actual serialized payload, including nested required fields,
		// against the exact Worker protocol shape rather than a permissive decoder.
		var report map[string]json.RawMessage
		if err := json.Unmarshal(raw, &report); err != nil {
			t.Error(err)
		}
		if len(report) != 7 {
			t.Error("unexpected envelope fields")
		}
		expected := `[{"kind":"account","network":"network","account":"User😀@test","upload":{"attempted":15,"admitted":10,"effective":{"mode":"limited","rate":10}},"download":{"attempted":0,"admitted":0,"effective":{"mode":"deny"}}}]`
		if string(report["entries"]) != expected {
			t.Errorf("incorrect entry shape: %s", report["entries"])
		}
		fmt.Fprintf(w, `{"version":1,"nodeId":"node","bootId":%s,"acceptedSeq":%s,"generation":1,"policyRevision":1,"allocations":[]}`, report["bootId"], report["seq"])
	}))
	defer server.Close()
	client, err := newCoordinatorClient(coordinatorTestConfig(server.URL), "synthetic-token", false)
	if err != nil {
		t.Fatal(err)
	}
	if client.http.Timeout != 5*time.Second || coordinatorInterval != 30*time.Second {
		t.Fatal("production timing changed")
	}
	client.http.Transport = server.Client().Transport
	if err := client.report(context.Background(), source); err != nil {
		t.Fatal(err)
	}
	if source.applied.Load() != 1 {
		t.Fatal("response not consumed")
	}
}

func TestCoordinatorClientInvalidWindowsAndSequenceExhaustion(t *testing.T) {
	client, err := newCoordinatorClient(coordinatorTestConfig("https://example.test"), "synthetic-token", false)
	if err != nil {
		t.Fatal(err)
	}
	source := &coordinatorWindowSource{invalid: true}
	if err := client.report(context.Background(), source); err == nil || client.seq != 0 {
		t.Fatal("invalid window sent")
	}
	source.invalid = false
	client.seq = coordinatorMaxSeq
	if err := client.report(context.Background(), source); err == nil || client.seq != coordinatorMaxSeq {
		t.Fatal("sequence wrapped")
	}
	source.entries = []derpserver.CoordinatorEntry{{CoordinatorSubject: derpserver.CoordinatorSubject{Kind: "account", Network: "network", Account: strings.Repeat("x", derpserver.CoordinatorMaxBody)}}}
	client.seq = 0
	if err := client.report(context.Background(), source); err == nil {
		t.Fatal("oversize report sent")
	}
}

func TestCoordinatorStartupLifecycle(t *testing.T) {
	old := *tailnetCoordinatorConfigPath
	t.Cleanup(func() { *tailnetCoordinatorConfigPath = old })
	*tailnetCoordinatorConfigPath = ""
	if err := startTailnetCoordinator(context.Background(), nil); err != nil {
		t.Fatal("default not disabled")
	}
	*tailnetCoordinatorConfigPath = filepath.Join(t.TempDir(), "missing")
	if err := startTailnetCoordinator(context.Background(), nil); err == nil {
		t.Fatal("missing config accepted")
	}
	directory := t.TempDir()
	token, config, rates := filepath.Join(directory, "token"), filepath.Join(directory, "coordinator.json"), filepath.Join(directory, "rates.json")
	files := map[string]string{
		token:  "synthetic-token\n",
		config: fmt.Sprintf(`{"endpoint":"https://example.test/coordinator/report","nodeId":"node","tokenFile":%q}`, token),
		rates:  `{"default":{"upload_bytes_per_second":0,"download_bytes_per_second":0},"tailnets":{}}`,
	}
	for path, data := range files {
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	*tailnetCoordinatorConfigPath = config
	server := derpserver.New(key.NewNode(), t.Logf)
	defer server.Close()
	if err := startTailnetCoordinator(context.Background(), server); err == nil {
		t.Fatal("non-tailnet startup accepted")
	}
	server.SetVerifyClientURL("https://example.test/verify")
	server.SetVerifyClientURLFailOpen(false)
	if err := server.SetTailnetAccountIdentity(true); err != nil {
		t.Fatal(err)
	}
	if err := server.LoadAndApplyTailnetRateConfig(rates); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := startTailnetRateWatchers(ctx, server, rates); err != nil {
		t.Fatal(err)
	}
	if err := validateTailnetAccountMode("rates", true); err != nil {
		t.Fatal(err)
	}
	if err := validateTailnetAccountMode("rates", false); err == nil {
		t.Fatal("startup flag bypassed v2 guard")
	}
	if data, err := readCoordinatorFile(token, 1); err == nil || data != nil {
		t.Fatal("unbounded file read")
	}
}

func TestCoordinatorConfigStrictValues(t *testing.T) {
	base := `{"endpoint":"https://example.test/coordinator/report","nodeId":"node","tokenFile":"/synthetic/token"}`
	bad := []string{
		strings.Replace(base, `"nodeId":"node"`, `"nodeId":" node"`, 1),
		strings.Replace(base, `"nodeId":"node"`, fmt.Sprintf(`"nodeId":"node","node%cu0049d":"other"`, 92), 1),
		strings.Replace(base, `"tokenFile":"/synthetic/token"`, `"tokenFile":null`, 1),
		strings.Replace(base, `"endpoint":"https://example.test/coordinator/report"`, `"endpoint":12`, 1),
	}
	for _, data := range bad {
		if _, err := derpserver.ParseCoordinatorConfig([]byte(data)); err == nil {
			t.Fatal("invalid config accepted")
		}
	}
	if _, err := derpserver.ParseCoordinatorConfig(make([]byte, derpserver.CoordinatorMaxBody+1)); err == nil {
		t.Fatal("oversize config accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	client, err := newCoordinatorClient(coordinatorTestConfig("https://example.test"), "synthetic-token", false)
	if err != nil {
		t.Fatal(err)
	}
	source := new(coordinatorTestSource)
	client.run(ctx, source, make(chan time.Time))
	if source.snapshots.Load() != 0 {
		t.Fatal("canceled reporter started request")
	}
	ticks := make(chan time.Time)
	close(ticks)
	client.run(context.Background(), source, ticks)
	if source.snapshots.Load() != 0 {
		t.Fatal("closed ticker spun")
	}
}
