package derpserver

import (
	"fmt"
	"strings"
	"testing"
)

func TestCoordinatorWireRequiredTypes(t *testing.T) {
	base := string(responseJSON(allocationJSON("", `{"mode":"deny"}`, `{"mode":"deny"}`)))
	mutations := [][2]string{
		{`"nodeId":"node"`, `"nodeId":null`},
		{`"nodeId":"node"`, `"nodeId":"bad node"`},
		{`"bootId":"boot"`, `"bootId":""`},
		{`"bootId":"boot"`, `"bootId":"bad boot"`},
		{`"bootId":"boot"`, `"bootId":"` + strings.Repeat("x", 129) + `"`},
		{`"acceptedSeq":1`, `"acceptedSeq":0`},
		{`"kind":"network"`, `"kind":false`},
		{`"kind":"network"`, `"kind":"unknown"`},
		{`"network":"network"`, `"network":null`},
		{`"network":"network"`, `"network":"bad network"`},
		{`"mode":"deny"`, `"mode":null`},
		{`"mode":"deny"`, `"mode":"other"`},
		{`"allocations":[{"kind":"network","network":"network","upload":{"mode":"deny"},"download":{"mode":"deny"}}]`, `"allocations":null`},
	}
	for i, pair := range mutations {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			input := strings.Replace(base, pair[0], pair[1], 1)
			if input == base {
				t.Fatal("test mutation missed fixture")
			}
			if _, err := parseCoordinatorResponse([]byte(input)); err == nil {
				t.Fatal("invalid type accepted")
			}
		})
	}
	r, _, _ := coordinatorRegistry(t)
	server := &Server{tailnetRate: r, tailnetAccountIdentity: true}
	if _, err := server.ApplyTailnetCoordinatorResponse([]byte(base), "wrong", "boot", 1); err == nil {
		t.Fatal("public API accepted binding mismatch")
	}
}

func TestCoordinatorConfigParserContract(t *testing.T) {
	base := `{"endpoint":"https://example.test/coordinator/report","nodeId":"node","tokenFile":"/synthetic/token"}`
	cfg, err := ParseCoordinatorConfig([]byte(base))
	if err != nil || cfg.Endpoint != "https://example.test/coordinator/report" || cfg.NodeID != "node" || cfg.TokenFile != "/synthetic/token" {
		t.Fatalf("config contract: %+v %v", cfg, err)
	}
	for _, input := range []string{
		`{}`, base + `{}`, strings.Replace(base, `"nodeId":"node"`, `"nodeId":"bad node"`, 1),
		strings.Replace(base, `"tokenFile":"/synthetic/token"`, `"tokenFile":null`, 1),
		strings.Replace(base, `"tokenFile":"/synthetic/token"`, `"tokenFile":""`, 1),
		strings.Replace(base, `"endpoint":"https://example.test/coordinator/report"`, `"endpoint":false`, 1),
		strings.Replace(base, `"nodeId":"node"`, fmt.Sprintf(`"nodeId":"node","node%cu0049d":"other"`, 92), 1),
	} {
		if _, err := ParseCoordinatorConfig([]byte(input)); err == nil {
			t.Fatal("invalid config accepted")
		}
	}
	if _, err := ParseCoordinatorConfig(make([]byte, CoordinatorMaxBody+1)); err == nil {
		t.Fatal("oversize config accepted")
	}
}
