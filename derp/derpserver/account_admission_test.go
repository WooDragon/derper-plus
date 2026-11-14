package derpserver

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/iotest"

	"tailscale.com/types/key"
)

// Load the exact shared wire fixture, not a Go-specific reconstruction.
func TestAccountAdmissionSharedFixture(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "identity-v2.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		NodePublic, Subject string
		Responses           []struct {
			Name, Raw string
			Valid     bool
		}
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	if len(fixture.Responses) != 24 {
		t.Fatalf("expected 24 shared wire responses, got %d", len(fixture.Responses))
	}
	var node key.NodePublic
	if err := node.UnmarshalText([]byte(fixture.NodePublic)); err != nil {
		t.Fatal(err)
	}
	for _, tc := range fixture.Responses {
		t.Run(tc.Name, func(t *testing.T) {
			id, err := parseAccountAdmission(strings.NewReader(tc.Raw), node)
			if (err == nil) != tc.Valid {
				t.Fatalf("valid=%v err=%v", tc.Valid, err)
			}
			if tc.Valid && (!id.valid() || id.network != fixture.Subject) {
				t.Fatal("incorrect trusted identity", id)
			}
		})
	}
	valid := fixture.Responses[0].Raw
	for _, tc := range []struct {
		name, raw string
		valid     bool
	}{
		{"exact 4KiB", valid + strings.Repeat(" ", 4096-len(valid)), true},
		{"over 4KiB", valid + strings.Repeat(" ", 4097-len(valid)), false},
		{"denied", `{"Allow":false}`, false},
		{"wrong allow type", strings.Replace(valid, `true`, `"true"`, 1), false},
		{"duplicate key", strings.Replace(valid, `"Kind":"user"`, `"Kind":"user","Key":"shared"`, 1), false},
		{"empty network", strings.Replace(valid, `network-0`, ``, 1), false},
		{"long network", strings.Replace(valid, `network-0`, strings.Repeat("n", 65), 1), false},
		{"missing allow alias", strings.Replace(valid, `"Allow":`, `"allow":`, 1), false},
		{"echo type", strings.Replace(valid, `"`+fixture.NodePublic+`"`, `42`, 1), false},
		{"network type", strings.Replace(valid, `"network-0"`, `42`, 1), false},
		{"kind type", strings.Replace(valid, `"user"`, `42`, 1), false},
		{"key type", strings.Replace(valid, `"Key":"h1_596e6e2f00ef312162a11e49aee4fefd8f46b9ad117dc26307bbe80c6401f987"`, `"Key":42`, 1), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseAccountAdmission(strings.NewReader(tc.raw), node)
			if (err == nil) != tc.valid {
				t.Fatal(err)
			}
		})
	}
}

func TestAccountAdmissionReadFailure(t *testing.T) {
	if _, err := parseAccountAdmission(iotest.ErrReader(io.ErrUnexpectedEOF), key.NewNode().Public()); err != io.ErrUnexpectedEOF {
		t.Fatalf("read failure not preserved: %v", err)
	}
}

func TestAccountSyntheticVectors(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "account-vectors.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Synthetic bool
		Secret    string
		Vectors   []struct{ NetworkSubject, User, Message, Key, ContextMessage, Context string }
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	if !fixture.Synthetic || len(fixture.Vectors) != 6 {
		t.Fatal("not six synthetic vectors")
	}
	secret, err := hex.DecodeString(fixture.Secret)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range fixture.Vectors {
		t.Run(v.User+"/"+v.NetworkSubject, func(t *testing.T) {
			message, err := json.Marshal([]string{"derp-account-v1", v.NetworkSubject, v.User})
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(message, []byte(v.Message)) {
				t.Fatal("message encoding drift")
			}
			mac := hmac.New(sha256.New, secret)
			mac.Write(message)
			if got := "h1_" + hex.EncodeToString(mac.Sum(nil)); got != v.Key || !validAccountKey(got) {
				t.Fatal("cross-language key drift")
			}
			mac.Reset()
			mac.Write([]byte(v.ContextMessage))
			if hex.EncodeToString(mac.Sum(nil)) != v.Context {
				t.Fatal("context drift")
			}
		})
	}
}
