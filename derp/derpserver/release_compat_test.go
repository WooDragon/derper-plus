// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

package derpserver

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"sync/atomic"
	"testing"

	"tailscale.com/derp"
	"tailscale.com/types/key"
)

// A blocked app must never reach either admission backend; mesh still bypasses
// both app filtering and identity lookup, regardless of the selected rate mode.
func TestReleaseAppFilterBeforeAdmission(t *testing.T) {
	for _, tailnet := range []bool{false, true} {
		for _, tc := range []struct {
			name        string
			mesh, allow bool
			calls       int32
		}{
			{"blocked", false, false, 0}, {"allowed", false, true, 1}, {"blocked mesh", true, true, 0},
		} {
			t.Run(fmt.Sprintf("tailnet=%v/%s", tailnet, tc.name), func(t *testing.T) {
				node := key.NewNode().Public()
				var calls atomic.Int32
				callback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					fmt.Fprintf(w, `{"Allow":true,"IdentityVersion":1,"Identity":{"NodePublic":%q,"RateLimitSubject":"network"}}`, node.String())
				}))
				defer callback.Close()
				s := New(key.NewNode(), t.Logf)
				defer s.Close()
				s.SetVerifyClientURL(callback.URL)
				s.SetVerifyClientURLFailOpen(false)
				s.SetDisallowedAppNames([]string{"blocked"})
				if err := s.SetMeshKey(testMeshKey); err != nil {
					t.Fatal(err)
				}
				if tailnet {
					s.tailnetRate = newTailnetRateRegistry(tailnetTestConfig(t, 1, 1, derp.MaxPacketSize), s.clock.Now)
				}
				info := &derp.ClientInfo{AppName: "blocked"}
				if tc.name == "allowed" {
					info.AppName = "allowed"
				}
				if tc.mesh {
					var err error
					info.MeshKey, err = key.ParseDERPMesh(testMeshKey)
					if err != nil {
						t.Fatal(err)
					}
				}
				_, err := s.verifyClient(context.Background(), node, info, netip.MustParseAddr("127.0.0.1"))
				if (err == nil) != tc.allow {
					t.Fatalf("allow=%v err=%v", tc.allow, err)
				}
				if got := calls.Load(); got != tc.calls {
					t.Fatalf("callback calls=%d want %d", got, tc.calls)
				}
			})
		}
	}
}

// An accepted payload belongs to the queue until writePending consumes it.
// Later upload drops and download drops must neither corrupt it nor emit frames.
func TestReleaseQueuedPayloadSurvivesPolicing(t *testing.T) {
	for _, tailnet := range []bool{false, true} {
		t.Run(fmt.Sprintf("tailnet=%v", tailnet), func(t *testing.T) {
			s := newUserRateTestServer(t)
			policy := userRatePolicy{uploadBytesPerSecond: 1, downloadBytesPerSecond: 1, burstBytes: derp.MaxPacketSize}
			s.userRate = newUserRateRegistry(userRateConfig{defaultPolicy: policy, taggedPolicy: policy})
			src := newUserRateTestClient(t, s, userRateSubject{userID: 1}, false)
			dst := newUserRateTestClient(t, s, userRateSubject{userID: 2}, false)
			if tailnet {
				s.tailnetRate = newTailnetRateRegistry(tailnetTestConfig(t, 1, 1, derp.MaxPacketSize), s.clock.Now)
				src.tailnetRateSubject, dst.tailnetRateSubject = "source", "destination"
				if !s.tailnetRate.admit("source") || !s.tailnetRate.admit("destination") {
					t.Fatal("admission")
				}
			}
			storeUserRateTestClient(s, dst)
			payload := bytes.Repeat([]byte{0x51}, derp.MaxPacketSize)
			src.br = userRateTestPacketReader(dst.key, payload)
			if err := src.handleFrameSendPacket(derp.FrameSendPacket, derp.KeyLen+derp.MaxPacketSize); err != nil {
				t.Fatal(err)
			}
			src.br = userRateTestPacketReader(dst.key, bytes.Repeat([]byte{0x72}, derp.MaxPacketSize))
			if err := src.handleFrameSendPacket(derp.FrameSendPacket, derp.KeyLen+derp.MaxPacketSize); err != nil {
				t.Fatal(err)
			}
			// Force other pool consumers to overwrite available payload buffers.
			other := s.getPacketBuf(derp.MaxPacketSize)
			for i := range *other {
				(*other)[i] = 0x93
			}
			defer s.putPacketBuf(other)
			queued, more, ok := dst.sendQueue.dequeue(s)
			if !ok || more || queued.buf == nil || !bytes.Equal(queued.bs, payload) {
				t.Fatal("queued payload lost or corrupted")
			}
			if dropped, _, ok := dst.sendQueue.enqueue(s, queued); !ok || dropped.bs != nil {
				t.Fatal("requeue")
			}
			var wire bytes.Buffer
			dst.bw = &lazyBufioWriter{w: &wire}
			if frames, carry, err := dst.writePending(pendSendQueue); err != nil || frames != 1 || carry != 0 {
				t.Fatalf("writePending: %d %v %v", frames, carry, err)
			}
			if err := dst.bw.Flush(); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(wire.Bytes()[len(wire.Bytes())-len(payload):], payload) {
				t.Fatal("wrong delivered payload")
			}
			size := wire.Len()
			mesh := newUserRateTestClient(t, s, userRateSubject{}, true)
			mesh.br = userRateTestForwardPacketReader(src.key, dst.key, payload)
			if err := mesh.handleFrameForwardPacket(derp.FrameForwardPacket, 2*derp.KeyLen+derp.MaxPacketSize); err != nil {
				t.Fatal(err)
			}
			if frames, carry, err := dst.writePending(pendSendQueue); err != nil || frames != 1 || carry != 0 {
				t.Fatalf("drop writePending: %d %v %v", frames, carry, err)
			}
			if err := dst.bw.Flush(); err != nil {
				t.Fatal(err)
			}
			if wire.Len() != size {
				t.Fatal("download drop emitted a frame")
			}
			if s.userRateUploadDropped.Value() != 1 || s.userRateDownloadDropped.Value() != 1 || s.userRateUploadBytes.Value() != derp.MaxPacketSize || s.userRateDownloadBytes.Value() != derp.MaxPacketSize {
				t.Fatal("policing accounting changed")
			}
		})
	}
}
