package derpserver

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"tailscale.com/derp"
	"tailscale.com/types/key"
)

func writeAccountPolicy(t *testing.T, path, data string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestAccountCapabilityAPI(t *testing.T) {
	cfg := accountConfig("1", accountLeaf(1, 1))
	for _, where := range []string{"network", "default"} {
		t.Run("disabled/"+where, func(t *testing.T) {
			input := cfg
			if where == "default" {
				input = `{"tailnets":{},"default":` + strings.TrimSuffix(strings.TrimPrefix(cfg, `{"tailnets":{"network":`), `}}`) + `}`
			}
			parseAccountConfig(t, input) // This must be a valid config rejected by capability, not syntax.
			s := New(key.NewNode(), t.Logf)
			defer s.Close()
			path := filepath.Join(t.TempDir(), "config.json")
			writeAccountPolicy(t, path, input)
			if s.LoadAndApplyTailnetRateConfig(path) == nil || s.tailnetRate != nil {
				t.Fatal("disabled capability accepted accounts")
			}
			writeAccountPolicy(t, path, `{"tailnets":{}}`)
			if err := s.LoadAndApplyTailnetRateConfig(path); err != nil {
				t.Fatal(err)
			}
			if s.SetTailnetAccountIdentity(true) == nil || s.SetTailnetAccountIdentity(false) == nil {
				t.Fatal("registry did not freeze capability")
			}
			writeAccountPolicy(t, path, input)
			if s.LoadAndApplyTailnetRateConfig(path) == nil {
				t.Fatal("reload enabled accounts")
			}
			if s.tailnetRate.config.hasAccounts() {
				t.Fatal("failed reload published accounts")
			}
		})
	}
	t.Run("enabled without tailnet", func(t *testing.T) {
		s := New(key.NewNode(), t.Logf)
		defer s.Close()
		if err := s.SetTailnetAccountIdentity(true); err != nil {
			t.Fatal(err)
		}
		if _, err := s.verifyClient(context.Background(), key.NewNode().Public(), &derp.ClientInfo{}, netip.MustParseAddr("127.0.0.1")); err == nil {
			t.Fatal("standalone capability admitted legacy")
		}
		path := filepath.Join(t.TempDir(), "config.json")
		writeAccountPolicy(t, path, cfg)
		if s.LoadAndApplyTailnetRateConfig(path) == nil {
			t.Fatal("invalid external admission mode accepted")
		}
	})
	t.Run("accept freezes even without registry", func(t *testing.T) {
		s := New(key.NewNode(), t.Logf)
		defer s.Close()
		conn, peer := net.Pipe()
		done := make(chan struct{})
		go func() {
			s.Accept(context.Background(), conn, bufio.NewReadWriter(bufio.NewReader(conn), bufio.NewWriter(conn)), "127.0.0.1:1")
			close(done)
		}()
		// Read the initial server frame: Accept has entered before trying to mutate.
		frame := make([]byte, 5)
		if _, err := io.ReadFull(peer, frame); err != nil {
			t.Fatal(err)
		}
		if s.SetTailnetAccountIdentity(true) == nil {
			t.Fatal("Accept did not freeze capability")
		}
		path := filepath.Join(t.TempDir(), "config.json")
		writeAccountPolicy(t, path, `{"tailnets":{}}`)
		if s.LoadAndApplyTailnetRateConfig(path) == nil {
			t.Fatal("Accept allowed mode initialization")
		}
		peer.Close()
		<-done
	})
}
