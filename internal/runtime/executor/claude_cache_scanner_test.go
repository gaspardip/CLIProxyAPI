package executor

import (
	"strings"
	"testing"

	"github.com/MoeForge/moego/ojson"
)

func TestClaudeStreamDocScannerStringBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name     string
		text     string
		nonEmpty bool
	}{
		{"plain", strings.Repeat("plain", 64), true},
		{"escaped delimiters", `prefix\"}],\"cache_control\":{\"type\":\"ephemeral\"}\\\"tail\\`, true},
		{"many backslashes", `prefix` + strings.Repeat(`\\`, 128) + `\u0020tail`, true},
		{"escaped whitespace", ` \t\r\n\u3000`, false},
		{"unicode whitespace", "　\u00a0", false},
		{"unicode text", "　é世界", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cc := `{"type":"ephemeral","ttl":"1h"}`
			raw := []byte(`{"tools":[{"name":"tool","input_schema":{"nested":[{"text":"` + tc.text + `","cache_control":{"type":"ignored"}}]},"cache_control":` + cc + `}],"messages":[{"role":"user","content":[{"type":"text","text":"` + tc.text + `","cache_control":` + cc + `}]},{"role":"assistant","content":[{"type":"thinking","thinking":"` + tc.text + `"}]},{"role":"system","content":"` + tc.text + `"}]}`)
			check := func(parts ...[]byte) {
				t.Helper()
				var scanner claudeStreamDocScanner
				scanner.scan(ojson.NewDoc(nil), true, false)
				for _, part := range parts {
					if n, err := scanner.Write(part); n != len(part) || err != nil {
						t.Fatalf("Write = (%d, %v), want (%d, nil)", n, err, len(part))
					}
				}
				if len(scanner.toolCC) != 1 || len(scanner.sysCC) != 0 || len(scanner.msgCC) != 1 {
					t.Fatalf("cache controls: tools=%d system=%d messages=%d, want 1/0/1", len(scanner.toolCC), len(scanner.sysCC), len(scanner.msgCC))
				}
				if got := scanner.toolCC[0]; got.idx1 != 0 || string(got.raw) != cc {
					t.Fatalf("tool cache control = %+v, want index 0 and %s", got, cc)
				}
				if got := scanner.msgCC[0]; got.idx1 != 0 || got.idx2 != 0 || string(got.raw) != cc {
					t.Fatalf("message cache control = %+v, want indices 0/0 and %s", got, cc)
				}
				if scanner.messagesLen != 3 || scanner.firstUserIdx != 0 || scanner.lastMessageIndex != 2 || scanner.lastMsgRole != "system" {
					t.Fatalf("messages: count=%d first user=%d last=%d role=%s", scanner.messagesLen, scanner.firstUserIdx, scanner.lastMessageIndex, scanner.lastMsgRole)
				}
				wantEligible := -1
				if tc.nonEmpty {
					wantEligible = 0
				}
				if scanner.lastEligibleIndex != wantEligible || scanner.lastMsgContentIsNonEmptyString != tc.nonEmpty || scanner.lastEligibleHasCacheControl != tc.nonEmpty {
					t.Fatalf("eligibility: index=%d final string nonempty=%v cached=%v, want %d/%v/%v", scanner.lastEligibleIndex, scanner.lastMsgContentIsNonEmptyString, scanner.lastEligibleHasCacheControl, wantEligible, tc.nonEmpty, tc.nonEmpty)
				}
			}
			// Every possible two-part boundary includes quotes, escapes, UTF-8,
			// nested containers and captured cache_control values.
			for split := 0; split <= len(raw); split++ {
				check(raw[:split], raw[split:])
			}
			parts := make([][]byte, len(raw))
			for i := range raw {
				parts[i] = raw[i : i+1]
			}
			check(parts...)
		})
	}
}

func TestClaudeStreamDocScannerUnterminatedString(t *testing.T) {
	for _, tail := range []string{`plain`, `escaped\"}]}`, `backslash\`, `unicode\u00`, `invalid\q\uxxxx`} {
		t.Run(tail, func(t *testing.T) {
			cc := `{"type":"ephemeral"}`
			raw := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"ok","cache_control":` + cc + `}]},{"role":"assistant","content":"` + tail)
			for split := 0; split <= len(raw); split++ {
				var scanner claudeStreamDocScanner
				scanner.scan(ojson.NewDoc(nil), true, false)
				for _, part := range [][]byte{raw[:split], raw[split:]} {
					if n, err := scanner.Write(part); n != len(part) || err != nil {
						t.Fatalf("split %d: Write = (%d, %v), want (%d, nil)", split, n, err, len(part))
					}
				}
				if len(scanner.msgCC) != 1 || string(scanner.msgCC[0].raw) != cc || scanner.lastMessageIndex != 0 || scanner.lastEligibleIndex != 0 {
					t.Fatalf("split %d: unfinished string changed completed message: cache controls=%+v last=%d eligible=%d", split, scanner.msgCC, scanner.lastMessageIndex, scanner.lastEligibleIndex)
				}
			}
		})
	}
}
