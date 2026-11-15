package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"tailscale.com/derp/derpserver"
)

type coordinatorTestSource struct {
	snapshots atomic.Int32
	applied   atomic.Int32
}

func (s *coordinatorTestSource) TailnetCoordinatorSnapshot() ([]derpserver.CoordinatorEntry, int64, error) {
	s.snapshots.Add(1)
	return []derpserver.CoordinatorEntry{}, 30000, nil
}
func (s *coordinatorTestSource) ApplyTailnetCoordinatorResponse(raw []byte, node, boot string, seq uint64) (uint64, error) {
	var response struct {
		Version int    `json:"version"`
		Node    string `json:"nodeId"`
		Boot    string `json:"bootId"`
		Seq     uint64 `json:"acceptedSeq"`
	}
	if json.Unmarshal(raw, &response) != nil || response.Version != 1 || response.Node != node || response.Boot != boot || response.Seq != seq {
		return 0, fmt.Errorf("binding")
	}
	s.applied.Add(1)
	return 1, nil
}
func coordinatorTestConfig(endpoint string) derpserver.CoordinatorConfig {
	return derpserver.CoordinatorConfig{Endpoint: endpoint + "/coordinator/report", NodeID: "node", TokenFile: "unused"}
}

// Given an explicit loopback transport, the reporter sends the exact Worker
// envelope, bearer authentication, and fresh windows with monotonically increasing seq.
func TestCoordinatorClientWireAndFailures(t *testing.T) {
	var requests atomic.Int32
	var boots []string
	var starts []int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != "POST" || req.URL.Path != "/coordinator/report" || req.Header.Get("Authorization") != "Bearer synthetic-token" {
			t.Error("incorrect request contract")
		}
		var report derpserver.CoordinatorReport
		if err := json.NewDecoder(req.Body).Decode(&report); err != nil {
			t.Error(err)
		}
		number := requests.Add(1)
		if report.Version != 1 || report.NodeID != "node" || report.WindowMS != 30000 || report.Seq != uint64(number) || report.Entries == nil {
			t.Error("incorrect report envelope")
		}
		boots = append(boots, report.BootID)
		starts = append(starts, report.BootStartedAtMS)
		if number == 2 {
			w.WriteHeader(503)
			return
		}
		fmt.Fprintf(w, `{"version":1,"nodeId":%q,"bootId":%q,"acceptedSeq":%d,"generation":1,"policyRevision":1,"allocations":[]}`, report.NodeID, report.BootID, report.Seq)
	}))
	defer server.Close()
	source := new(coordinatorTestSource)
	client, err := newCoordinatorClient(coordinatorTestConfig(server.URL), "synthetic-token", true)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		err := client.report(context.Background(), source)
		if (i == 1) != (err != nil) {
			t.Fatalf("request %d error: %v", i, err)
		}
	}
	if source.snapshots.Load() != 3 || source.applied.Load() != 2 {
		t.Fatal("failed window retried or applied")
	}
	if boots[0] == "" || boots[0] != boots[2] || starts[0] <= 0 || starts[0] != starts[2] {
		t.Fatal("boot changed between requests")
	}
}

func TestCoordinatorClientURLAndConfig(t *testing.T) {
	for _, endpoint := range []string{"http://example.test/coordinator/report", "https://user:pass@example.test/coordinator/report", "https://example.test/coordinator/report?token=secret", "https://example.test/coordinator/report#fragment", "https://example.test/verify", "ftp://example.test/coordinator/report", "http://127.0.0.1:80/coordinator/report"} {
		cfg := coordinatorTestConfig("https://example.test")
		cfg.Endpoint = endpoint
		if _, err := newCoordinatorClient(cfg, "synthetic-token", false); err == nil {
			t.Fatalf("unsafe endpoint accepted: %s", endpoint)
		}
	}
	for _, token := range []string{"", "a\nb", "a\rb", "a b", "a\x00b"} {
		if _, err := newCoordinatorClient(coordinatorTestConfig("https://example.test"), token, false); err == nil {
			t.Fatal("invalid bearer accepted")
		}
	}
	directory := t.TempDir()
	token := filepath.Join(directory, "token")
	config := filepath.Join(directory, "coordinator.json")
	if err := os.WriteFile(token, []byte("synthetic-token\n"), 0600); err != nil {
		t.Fatal(err)
	}
	data := fmt.Sprintf(`{"endpoint":"https://example.test/coordinator/report","nodeId":"node","tokenFile":%q}`, token)
	if err := os.WriteFile(config, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadCoordinatorClient(config); err != nil {
		t.Fatal(err)
	}
	for _, data := range []string{`{}`, `{"endpoint":"https://example.test/coordinator/report","nodeId":"node","tokenFile":"missing","unknown":1}`, strings.Replace(data, `"nodeId":"node"`, `"nodeId":"node","nodeId":"other"`, 1)} {
		if _, err := derpserver.ParseCoordinatorConfig([]byte(data)); err == nil {
			t.Fatal("bad config accepted")
		}
	}
	if err := validateTailnetCoordinatorMode("config", "", true); err == nil {
		t.Fatal("coordinator without tailnet")
	}
	if err := validateTailnetCoordinatorMode("config", "rates", false); err == nil {
		t.Fatal("coordinator without v2")
	}
	if err := validateTailnetCoordinatorMode("", "", false); err != nil {
		t.Fatal("default disabled changed")
	}
}

func TestCoordinatorClientRedirectBodyAndCancellation(t *testing.T) {
	var targetHits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { targetHits.Add(1) }))
	defer target.Close()
	for _, mode := range []string{"redirect", "oversize", "unauthorized", "malformed", "timeout", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			entered := make(chan struct{}, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if _, err := io.Copy(io.Discard, req.Body); err != nil {
					t.Error(err)
				}
				switch mode {
				case "redirect":
					http.Redirect(w, req, target.URL, 307)
				case "oversize":
					fmt.Fprint(w, strings.Repeat("x", derpserver.CoordinatorMaxBody+1))
				case "unauthorized":
					w.WriteHeader(http.StatusUnauthorized)
				case "malformed":
					fmt.Fprint(w, `{}`)
				default:
					entered <- struct{}{}
					<-req.Context().Done()
				}
			}))
			defer server.Close()
			client, err := newCoordinatorClient(coordinatorTestConfig(server.URL), "synthetic-token", true)
			if err != nil {
				t.Fatal(err)
			}
			client.http.Timeout = 50 * time.Millisecond
			source := new(coordinatorTestSource)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if mode == "cancel" {
				go func() { <-entered; cancel() }()
			}
			if err := client.report(ctx, source); err == nil {
				t.Fatal("failure accepted")
			}
			if source.applied.Load() != 0 {
				t.Fatal("failure applied")
			}
		})
	}
	if targetHits.Load() != 0 {
		t.Fatal("redirect leaked bearer")
	}
}

func TestCoordinatorClientSingleFlightAndShutdown(t *testing.T) {
	entered, release := make(chan struct{}, 2), make(chan struct{})
	var inFlight, maximum atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if _, err := io.Copy(io.Discard, req.Body); err != nil {
			t.Error(err)
		}
		count := inFlight.Add(1)
		defer inFlight.Add(-1)
		if count > maximum.Load() {
			maximum.Store(count)
		}
		entered <- struct{}{}
		select {
		case <-release:
			w.WriteHeader(503)
		case <-req.Context().Done():
		}
	}))
	defer server.Close()
	client, err := newCoordinatorClient(coordinatorTestConfig(server.URL), "synthetic-token", true)
	if err != nil {
		t.Fatal(err)
	}
	source := new(coordinatorTestSource)
	ticks := make(chan time.Time, 10)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); client.run(ctx, source, ticks) }()
	ticks <- time.Now()
	<-entered
	for i := 0; i < 5; i++ {
		ticks <- time.Now()
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("shutdown failed to cancel request")
	}
	close(release)
	if maximum.Load() != 1 || source.snapshots.Load() != 1 {
		t.Fatal("queued requests or multiple in flight")
	}
}
