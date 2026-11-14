package derpserver

import (
	"fmt"
	"strings"
	"testing"
)

func accountLeaf(up, down int) string {
	return fmt.Sprintf(`{"upload_bytes_per_second":%d,"download_bytes_per_second":%d}`, up, down)
}

func accountConfig(parent, child string) string {
	return `{"tailnets":{"network":{"upload_bytes_per_second":` + parent + `,"download_bytes_per_second":` + parent + `,"accounts":{"default":` + child + `}}}}`
}

func TestAccountPlainOverride(t *testing.T) {
	user := "合成.User+\"别名\\@example.test"
	leaf := accountLeaf(3750000, 3750000)
	raw := fmt.Sprintf(`{"tailnets":{"network":{"upload_bytes_per_second":0,"download_bytes_per_second":0,"accounts":{"default":%s,"overrides":{%q:%s}}}}}`, leaf, user, accountLeaf(0, 0))
	cfg := parseAccountConfig(t, raw)
	policy, ok := cfg.accountPolicyFor(accountID("network", "user", user))
	if !ok || policy.upload != 0 || policy.download != 0 {
		t.Fatal("exact plain account override did not match")
	}
	for _, other := range []string{strings.ToLower(user), "other@example.test"} {
		policy, ok := cfg.accountPolicyFor(accountID("network", "user", other))
		if !ok || policy.upload != 3750000 || policy.download != 3750000 {
			t.Fatal("override leaked or default changed")
		}
	}
}

// Account policy accepts exact registering users, not network-subject syntax.
func TestAccountPolicySchema(t *testing.T) {
	leaf := accountLeaf(1, 0)
	valid := accountConfig("1", leaf)
	for _, tc := range []struct {
		name, accounts string
		valid          bool
	}{
		{"complete", `{"default":` + leaf + `}`, true},
		{"optional overrides", `{"default":` + leaf + `,"overrides":{}}`, true},
		{"override", `{"default":` + leaf + `,"overrides":{"User+alias@example.test":` + leaf + `}}`, true},
		{"null", `null`, false},
		{"empty", `{}`, false},
		{"obsolete tagged leaf", `{"default":` + leaf + `,"tagged":` + leaf + `}`, false},
		{"missing default", `{"overrides":{}}`, false},
		{"recursive leaf", `{"default":{"upload_bytes_per_second":1,"download_bytes_per_second":0,"accounts":{}}}`, false},
		{"bad override", `{"default":` + leaf + `,"overrides":{" user":` + leaf + `}}`, false},
		{"partial override", `{"default":` + leaf + `,"overrides":{"user":{"upload_bytes_per_second":1}}}`, false},
		{"null overrides", `{"default":` + leaf + `,"overrides":null}`, false},
		{"unknown", `{"default":` + leaf + `,"other":0}`, false},
		{"duplicate", `{"default":` + leaf + `,"default":` + leaf + `}`, false},
		{"duplicate decoded account", `{"default":` + leaf + `,"overrides":{"u@example.test":` + leaf + `,"\u0075@example.test":` + leaf + `}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := strings.Replace(valid, `{"default":`+leaf+`}`, tc.accounts, 1)
			_, err := parseTailnetRateConfig([]byte(input))
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v err=%v", tc.valid, err)
			}
			if tc.name == "duplicate decoded account" && strings.Contains(err.Error(), "u@example.test") {
				t.Fatal("account leaked in reload diagnostic")
			}
		})
	}
}
