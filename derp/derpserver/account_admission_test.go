package derpserver

import (
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
		{"key type", strings.Replace(valid, `"Key":"Synthetic.User@example.test"`, `"Key":42`, 1), false},
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

func TestAccountUserValidation(t *testing.T) {
	for _, user := range []string{"User+alias@example.test", "合成\"用户\\@example.test", "é", "é", strings.Repeat("雪", 341) + "a", "shared"} {
		if !validAccountKey(user) {
			t.Fatal("valid registering user rejected")
		}
	}
	for _, user := range []string{"", " leading", "trailing ", "a\nb", "a\u007fb", "a\u0085b", "\uFEFFuser", strings.Repeat("雪", 342), string([]byte{0xff})} {
		if validAccountKey(user) {
			t.Fatal("invalid registering user accepted")
		}
	}
}

// Both callback values and override names must decode without replacement.
func TestAccountJSONUnicode(t *testing.T) {
	node := key.NewNode().Public()
	for _, tc := range []struct {
		name, raw, want string
		valid           bool
	}{
		{"isolated high", `"\ud800@example.test"`, "", false},
		{"different isolated high", `"\ud801@example.test"`, "", false},
		{"isolated low", `"\udc00@example.test"`, "", false},
		{"surrogate pair", `"\ud83d\ude00@example.test"`, "😀@example.test", true},
		{"literal replacement", `"�@example.test"`, "�@example.test", true},
		{"escaped replacement", `"\ufffd@example.test"`, "�@example.test", true},
		{"literal escape text", `"\\ud800@example.test"`, `\ud800@example.test`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wire := `{"Allow":true,"IdentityVersion":2,"Identity":{"NodePublic":"` + node.String() + `","RateLimitSubject":"network","Account":{"Kind":"user","Key":` + tc.raw + `}}}`
			id, err := parseAccountAdmission(strings.NewReader(wire), node)
			if (err == nil) != tc.valid {
				t.Errorf("admission valid=%v err=%v key=%q", tc.valid, err, id.key)
			}
			if tc.valid && id.key != tc.want {
				t.Error("admission changed account")
			}
			policy := `{"tailnets":{"network":{"upload_bytes_per_second":0,"download_bytes_per_second":0,"accounts":{"default":` + accountLeaf(3750000, 3750000) + `,"overrides":{` + tc.raw + `:` + accountLeaf(0, 0) + `}}}}}`
			cfg, err := parseTailnetRateConfig([]byte(policy))
			if (err == nil) != tc.valid {
				t.Errorf("override valid=%v err=%v", tc.valid, err)
			}
			if tc.valid {
				leaf, ok := cfg.accountPolicyFor(accountID("network", "user", tc.want))
				if !ok || leaf.upload != 0 || leaf.download != 0 {
					t.Error("exact Unicode override did not match")
				}
			}
		})
	}
}
