// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

package derphttp_test

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"tailscale.com/derp"
)

func jointHTTPReady(url string) bool {
	probe := &http.Client{Timeout: 100 * time.Millisecond}
	response, err := probe.Get(strings.TrimSuffix(url, "/derp") + "/")
	if err != nil {
		return false
	}
	response.Body.Close()
	return response.StatusCode == 200
}

func jointWrite(t *testing.T, path, contents string) {
	t.Helper()
	temporary := path + ".new"
	if err := os.WriteFile(temporary, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(temporary, path); err != nil {
		t.Fatal(err)
	}
}

func (h *jointHarness) reload(t *testing.T, process *os.Process, config, ack string) {
	t.Helper()
	h.reloadAt(t, process, h.ready.Policy, filepath.Join(h.run, "binary.log"), config, ack)
}

func (h *jointHarness) reloadAt(t *testing.T, process *os.Process, policy, logPath, config, ack string) {
	t.Helper()
	before, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	jointWrite(t, policy, config)
	if err := process.Signal(syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	jointWait(t, func() bool {
		data, err := os.ReadFile(logPath)
		if err != nil {
			t.Fatal(err)
		}
		return len(data) >= len(before) && strings.Contains(string(data[len(before):]), ack)
	})
	t.Logf("actual SIGHUP PID=%d acknowledged: %s", process.Pid, ack)
}

func (h *jointHarness) sighup(t *testing.T) {
	process, err := os.FindProcess(h.ready.PID)
	if err != nil {
		t.Fatal(err)
	}
	defer process.Release()
	jointWait(t, func() bool { return jointHTTPReady(h.ready.BinaryURL) })
	t.Logf("actual binary PID=%d loopback=%s", process.Pid, h.ready.BinaryURL)
	first, free := h.client(t, h.ready.BinaryURL, "node0", true), h.client(t, h.ready.BinaryURL, "free", true)
	h.send(t, first, "free", 1, derp.MaxPacketSize)
	h.receive(t, free, "node0", 1, derp.MaxPacketSize)
	h.send(t, first, "free", 2, derp.MaxPacketSize)
	h.receive(t, free, "node0", 2, derp.MaxPacketSize)
	h.reload(t, process, jointConfig(1, 0), "tailnet rate config reloaded successfully")
	h.send(t, first, "free", 3, derp.MaxPacketSize)
	jointAbsent(t, free)
	h.send(t, free, "free", 4, derp.MaxPacketSize)
	h.receive(t, free, "free", 4, derp.MaxPacketSize)
	h.reload(t, process, `{"tailnets":{},"default":{"upload_bytes_per_second":-1}}`, "tailnet rate config reload failed")
	h.send(t, first, "free", 5, derp.MaxPacketSize)
	jointAbsent(t, free)
	h.send(t, free, "free", 6, derp.MaxPacketSize)
	h.receive(t, free, "free", 6, derp.MaxPacketSize)
	h.reload(t, process, jointAccountConfig(0, 0, 1, 0, true), "accounts require startup tailnet account identity capability")
	h.send(t, first, "free", 11, derp.MaxPacketSize)
	jointAbsent(t, free)
	h.send(t, free, "free", 12, derp.MaxPacketSize)
	h.receive(t, free, "free", 12, derp.MaxPacketSize)
	h.reload(t, process, jointConfig(0, 0), "tailnet rate config reloaded successfully")
	h.send(t, first, "free", 7, derp.MaxPacketSize)
	h.receive(t, free, "node0", 7, derp.MaxPacketSize)
	t.Logf("same authenticated connections obeyed valid/invalid/restored policy; binary=%s", h.ready.Binary)
}
