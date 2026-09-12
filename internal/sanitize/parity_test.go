package sanitize

import (
	"encoding/json"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// TestParity_Sanitize enforces byte/structure parity against goldens over a
// realistic corpus — never hand-written, never from unit toys.

type goldens struct {
	MasterKey  string `json:"masterKey"`
	Scope      string `json:"scope"`
	KeyVersion int    `json:"keyVersion"`
	Tokenize   []struct {
		Input  string      `json:"input"`
		Token  string      `json:"token"`
		Format TokenFormat `json:"format"`
		Hash   string      `json:"hash"`
	} `json:"tokenize"`
	Classify []struct {
		Key      string `json:"key"`
		Value    any    `json:"value"`
		IsHeader bool   `json:"isHeader"`
		Mode     Mode   `json:"mode"`
	} `json:"classify"`
	Sanitize []struct {
		Name         string      `json:"name"`
		IsHeaderRoot bool        `json:"isHeaderRoot"`
		Input        any         `json:"input"`
		Sanitized    any         `json:"sanitized"`
		Redactions   []Redaction `json:"redactions"`
	} `json:"sanitize"`
}

func loadGoldens(t *testing.T) goldens {
	t.Helper()
	raw, err := os.ReadFile("../../testdata/parity/sanitize/goldens.json")
	if err != nil {
		t.Fatalf("goldens missing — parity goldens are committed, not generated; restore them from git: %v", err)
	}
	var g goldens
	if err := json.Unmarshal(raw, &g); err != nil {
		t.Fatalf("goldens unreadable: %v", err)
	}
	return g
}

func TestParity_Tokenize(t *testing.T) {
	g := loadGoldens(t)
	tok := NewTokenizer(g.MasterKey, g.Scope, g.KeyVersion)
	for _, c := range g.Tokenize {
		token, format := tok.Tokenize(c.Input)
		if token != c.Token || format != c.Format {
			t.Errorf("tokenize(%q) = (%q,%s), golden = (%q,%s)", c.Input, token, format, c.Token, c.Format)
		}
		if h := tok.HashOf(c.Input); h != c.Hash {
			t.Errorf("hashOf(%q) = %s, golden = %s", c.Input, h, c.Hash)
		}
	}
}

func TestParity_Classify(t *testing.T) {
	g := loadGoldens(t)
	for _, c := range g.Classify {
		if knownStricterKeyRE.MatchString(c.Key) {
			continue // deliberate strictness divergence; see strictness_test.go
		}
		if mode := Classify(c.Key, c.Value, c.IsHeader); mode != c.Mode {
			t.Errorf("classify(%q,%v,header=%v) = %s, golden = %s", c.Key, c.Value, c.IsHeader, mode, c.Mode)
		}
	}
}

// knownStricterKeyRE: keys held to a stricter rule than the goldens encode —
// card-security/one-time secrets and expiry components are redacted
// REGARDLESS of JSON type, where the goldens let the numeric forms through.
// Fields under these keys are pruned from BOTH sides of the parity comparison
// here and asserted directly in strictness_test.go — the divergence is
// explicit, never silent.
var knownStricterKeyRE = regexp.MustCompile(`(?i)(^|_|-)(cvv2?|cvc2?|cid|csc|pin|otp|passcode|security[_-]?code|one[_-]?time[_-]?(code|password|pin)|expiry|expiration|exp[_-]?month|exp[_-]?year|valid[_-]?(thru|until)|birth|birthday)($|_|-)`)

func pruneStricterKeys(node any) any {
	switch n := node.(type) {
	case map[string]any:
		out := make(map[string]any, len(n))
		for k, v := range n {
			if knownStricterKeyRE.MatchString(k) {
				continue
			}
			out[k] = pruneStricterKeys(v)
		}
		return out
	case []any:
		out := make([]any, len(n))
		for i, v := range n {
			out[i] = pruneStricterKeys(v)
		}
		return out
	}
	return node
}

func pruneStricterRedactions(rs []Redaction) []Redaction {
	out := make([]Redaction, 0, len(rs))
	for _, r := range rs {
		segs := strings.Split(r.Pointer, "/")
		if knownStricterKeyRE.MatchString(segs[len(segs)-1]) {
			continue
		}
		out = append(out, r)
	}
	return out
}

func TestParity_Sanitize(t *testing.T) {
	g := loadGoldens(t)
	tok := NewTokenizer(g.MasterKey, g.Scope, g.KeyVersion)
	for _, c := range g.Sanitize {
		res := Sanitize(c.Input, tok, nil, c.IsHeaderRoot)
		got := pruneStricterKeys(normalize(t, res.Sanitized))
		want := pruneStricterKeys(normalize(t, c.Sanitized))
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: sanitized tree diverges from golden\n got: %v\nwant: %v", c.Name, got, want)
		}
		// Walk order differs across languages; redactions compare as a set.
		if gotR, wantR := redactionSet(pruneStricterRedactions(res.Redactions)), redactionSet(pruneStricterRedactions(c.Redactions)); !reflect.DeepEqual(gotR, wantR) {
			t.Errorf("%s: redactions diverge\n got: %v\nwant: %v", c.Name, gotR, wantR)
		}
	}
}

func normalize(t *testing.T, v any) any {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return out
}

func redactionSet(rs []Redaction) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.Pointer + "|" + string(r.Mode)
	}
	sort.Strings(out)
	return out
}
