// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

package derphttp_test

import (
	"context"
	"errors"
	"net"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"tailscale.com/derp"
	"tailscale.com/types/key"
)

// After admission, route actual DERP frame writes to an unread pipe. Unlike a
// sleeping mock Send, this exercises the transport's write-deadline contract.
type jointBlockedConn struct {
	net.Conn
	writer      net.Conn
	blocked     atomic.Bool
	writeErrors chan error
}

func (c *jointBlockedConn) Write(p []byte) (int, error) {
	if c.blocked.Load() {
		n, err := c.writer.Write(p)
		c.writeErrors <- err
		return n, err
	}
	return c.Conn.Write(p)
}

func (c *jointBlockedConn) SetWriteDeadline(deadline time.Time) error {
	if err := c.Conn.SetWriteDeadline(deadline); err != nil {
		return err
	}
	return c.writer.SetWriteDeadline(deadline)
}

func (c *jointBlockedConn) Close() error {
	return errors.Join(c.writer.Close(), c.Conn.Close())
}

func jointPulseClient(t *testing.T, wrap func(net.Conn) net.Conn) (*jointHarness, *jointClient) {
	t.Helper()
	h := &jointHarness{run: t.TempDir(), keys: map[string]key.NodePrivate{}, public: map[string]jointKey{}}
	h.addKey("free", "free")
	url := h.server(t, "", "", false)
	return h, h.clientWithConn(t, url, "free", true, wrap)
}

func TestAccountPulseBoundaries(t *testing.T) {
	for _, scenario := range []struct {
		name        string
		stopAfter   time.Duration
		finishAfter time.Duration
		want        int
	}{
		{"queued_ticks_after_stop", -time.Second, 150 * time.Millisecond, 0},
		{"observation_deadline_already_expired", time.Second, -time.Second, 0},
		{"finite_packet_and_byte_budget", time.Second, 150 * time.Millisecond, 8},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			h, sender := jointPulseClient(t, func(conn net.Conn) net.Conn { return conn })
			// A permanently ready tick channel models a delayed scheduler with
			// queued ticks, without relying on which select case wins first.
			ticks := make(chan time.Time)
			close(ticks)
			now := time.Now()
			sent, delivered, err := jointAccountPulse(sender, sender.packets, h.keys["free"].Public(), h.keys["free"].Public(), ticks, now.Add(scenario.stopAfter), now.Add(scenario.finishAfter))
			if err != nil || sent != scenario.want || delivered != scenario.want {
				t.Fatalf("sent=%d (%d payload bytes), delivered=%d, err=%v; want %d", sent, sent*derp.MaxPacketSize, delivered, err, scenario.want)
			}
			// The deadline must be cleared on a successful held connection.
			h.send(t, sender, "free", 42, derp.MaxPacketSize)
			h.receive(t, sender, "free", 42, derp.MaxPacketSize)
		})
	}
}

func TestAccountPulseBlockedSendDeadline(t *testing.T) {
	writer, reader := net.Pipe()
	t.Cleanup(func() { writer.Close(); reader.Close() })
	var blocked *jointBlockedConn
	h, sender := jointPulseClient(t, func(conn net.Conn) net.Conn {
		blocked = &jointBlockedConn{Conn: conn, writer: writer, writeErrors: make(chan error, 1)}
		return blocked
	})
	blocked.blocked.Store(true)
	ticks := make(chan time.Time, 1)
	ticks <- time.Now()
	// Rescue the broken implementation so a regression fails, rather than
	// hanging the suite or leaving a Send goroutine behind.
	rescue := time.AfterFunc(time.Second, func() { writer.Close() })
	defer rescue.Stop()
	start := time.Now()
	sent, _, err := jointAccountPulse(sender, sender.packets, h.keys["free"].Public(), h.keys["free"].Public(), ticks, start.Add(100*time.Millisecond), start.Add(200*time.Millisecond))
	if !errors.Is(err, context.DeadlineExceeded) || sent != 1 || time.Since(start) >= 750*time.Millisecond {
		t.Fatalf("blocked real Send: sent=%d elapsed=%v err=%v; want write deadline without rescue", sent, time.Since(start), err)
	}
	select {
	case err := <-blocked.writeErrors:
		if !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("actual frame Write returned %v, want transport deadline", err)
		}
	default:
		t.Fatal("real Send did not perform a blocked frame Write")
	}
}
