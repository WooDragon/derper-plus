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
	return `{"tailnets":{"network":{"upload_bytes_per_second":` + parent + `,"download_bytes_per_second":` + parent + `,"accounts":{"default":` + child + `,"tagged":` + child + `}}}}`
}

// Given an optional accounts object, require complete nonrecursive leaves and
// exact opaque override keys; network overrides must not inherit accounts.
func TestAccountPolicySchema(t *testing.T) {
	leaf := accountLeaf(1, 0)
	valid := accountConfig("1", leaf)
	for _, tc := range []struct {
		name, input string
		valid       bool
	}{
		{"complete", valid, true},
		{"zero", accountConfig("0", accountLeaf(0, 0)), true},
		{"optional overrides", strings.Replace(valid, `"tagged":`, `"overrides":{},"tagged":`, 1), true},
		{"override", strings.Replace(valid, `"tagged":`, `"overrides":{"h1_`+strings.Repeat("a", 64)+`":`+leaf+`},"tagged":`, 1), true},
		{"null", strings.Replace(valid, `{"default":`+leaf+`,"tagged":`+leaf+`}`, `null`, 1), false},
		{"empty", strings.Replace(valid, `{"default":`+leaf+`,"tagged":`+leaf+`}`, `{}`, 1), false},
		{"missing tagged", strings.Replace(valid, `,"tagged":`+leaf, ``, 1), false},
		{"missing default", strings.Replace(valid, `"default":`+leaf+`,`, ``, 1), false},
		{"recursive leaf", strings.Replace(valid, `"default":`+leaf, `"default":{"upload_bytes_per_second":1,"download_bytes_per_second":0,"accounts":{}}`, 1), false},
		{"bad override", strings.Replace(valid, `"tagged":`, `"overrides":{"user":`+leaf+`},"tagged":`, 1), false},
		{"partial override", strings.Replace(valid, `"tagged":`, `"overrides":{"h1_`+strings.Repeat("a", 64)+`":{"upload_bytes_per_second":1}},"tagged":`, 1), false},
		{"null overrides", strings.Replace(valid, `"tagged":`, `"overrides":null,"tagged":`, 1), false},
		{"unknown", strings.Replace(valid, `"tagged":`, `"other":0,"tagged":`, 1), false},
		{"duplicate", strings.Replace(valid, `"tagged":`, `"default":`+leaf+`,"tagged":`, 1), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseTailnetRateConfig([]byte(tc.input))
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v err=%v", tc.valid, err)
			}
		})
	}
}
