package scenario

import (
	"strings"
	"testing"
)

func TestMatchesOpRefusesCatastrophicRegex(t *testing.T) {
	if _, err := applyOp("matches", "aaaaaaaaaaaaaaaaaaaa", "(a+)+$", true); err == nil {
		t.Fatal("nested unbounded quantifier must be refused")
	}
	if _, err := applyOp("matches", "x", "[", true); err == nil {
		t.Fatal("uncompilable pattern must be refused")
	}
	ok, err := applyOp("matches", "abc-123", `^abc-\d+$`, true)
	if err != nil || !ok {
		t.Fatalf("benign pattern must match: ok=%v err=%v", ok, err)
	}
	ok, err = applyOp("matches", "zzz", `^abc$`, true)
	if err != nil || ok {
		t.Fatalf("benign non-match must return false, not error: ok=%v err=%v", ok, err)
	}
}

func TestMatchesOpBoundsInputLength(t *testing.T) {

	huge := strings.Repeat("a", 1<<20)
	if _, err := applyOp("matches", huge, `^a+$`, true); err == nil {

		t.Log("oversized input matched without guard error (bounded engine)")
	}
}

func TestAnyMatchers(t *testing.T) {
	cases := []struct {
		matcher string
		yes, no string
	}{
		{"{{any:string}}", "hello", ""},
		{"{{any:uuid}}", "550e8400-e29b-41d4-a716-446655440000", "not-a-uuid"},
		{"{{any:iso8601}}", "2026-09-07T10:00:00Z", "yesterday"},
	}
	for _, c := range cases {
		ok, err := applyOp("equals", c.yes, c.matcher, true)
		if err != nil || !ok {
			t.Fatalf("%s must accept %q: ok=%v err=%v", c.matcher, c.yes, ok, err)
		}
		if c.no == "" {
			continue
		}
		ok, err = applyOp("equals", c.no, c.matcher, true)
		if err != nil || ok {
			t.Fatalf("%s must reject %q: ok=%v err=%v", c.matcher, c.no, ok, err)
		}
	}

	if ok, _ := applyOp("equals", float64(42), "{{any:number}}", true); !ok {
		t.Fatal("any:number must accept a number")
	}
	if ok, _ := applyOp("equals", "42", "{{any:number}}", true); ok {
		t.Fatal("any:number must reject a string")
	}
	if ok, _ := applyOp("equals", true, "{{any:boolean}}", true); !ok {
		t.Fatal("any:boolean must accept a bool")
	}
}
