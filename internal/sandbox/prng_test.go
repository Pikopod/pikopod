package sandbox

import (
	"encoding/json"
	"math"
	"os"
	"testing"
)

func TestParity_Prng(t *testing.T) {
	raw, err := os.ReadFile("../../testdata/parity/prng/goldens.json")
	if err != nil {
		t.Fatalf("goldens missing — parity goldens are committed, not generated; restore them from git: %v", err)
	}
	var cases []struct {
		Seed    string    `json:"seed"`
		Next    []float64 `json:"next"`
		Int1100 []int     `json:"int_1_100"`
		Word    string    `json:"word"`
		Token8  string    `json:"token8"`
		Hex12   string    `json:"hex12"`
		Bool    bool      `json:"bool"`
		Pick    string    `json:"pick"`
	}
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		p := NewPrng(c.Seed)
		for i, want := range c.Next {
			if got := p.Next(); math.Abs(got-want) > 0 {
				t.Errorf("seed %q next[%d] = %.17g, golden = %.17g", c.Seed, i, got, want)
			}
		}
		p2 := NewPrng(c.Seed)
		for i, want := range c.Int1100 {
			if got := p2.Int(1, 100); got != want {
				t.Errorf("seed %q int[%d] = %d, golden = %d", c.Seed, i, got, want)
			}
		}
		if got := p2.Word(); got != c.Word {
			t.Errorf("seed %q word = %q, golden = %q", c.Seed, got, c.Word)
		}
		if got := p2.Token(8); got != c.Token8 {
			t.Errorf("seed %q token8 = %q, golden = %q", c.Seed, got, c.Token8)
		}
		if got := p2.Hex(12); got != c.Hex12 {
			t.Errorf("seed %q hex12 = %q, golden = %q", c.Seed, got, c.Hex12)
		}
		if got := p2.Bool(); got != c.Bool {
			t.Errorf("seed %q bool = %v, golden = %v", c.Seed, got, c.Bool)
		}
		if got := p2.Pick([]string{"success", "pending", "failed"}); got != c.Pick {
			t.Errorf("seed %q pick = %q, golden = %q", c.Seed, got, c.Pick)
		}
	}
}
