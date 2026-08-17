package arbiter

import (
	"testing"

	"github.com/lieyanc/BetterOCR/internal/agent"
)

func TestSimilarity(t *testing.T) {
	cases := []struct {
		a, b string
		want float64
	}{
		{"hello", "hello", 1},
		{"", "hello", 0},
		{"a", "a", 1},
		{"a", "b", 0},
	}
	for _, c := range cases {
		if got := Similarity(c.a, c.b); got != c.want {
			t.Errorf("Similarity(%q,%q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestFusePicksConsensus(t *testing.T) {
	a := arbiterNew()
	results := []agent.Result{
		{Agent: "a", Text: "Hello, BetterOCR!", Confidence: 0.92},
		{Agent: "b", Text: "Hello, BetterOCR!", Confidence: 0.88},
		{Agent: "c", Text: "He110, BetterOCR!", Confidence: 0.41},
	}
	final := a.Fuse(results)
	if final.Text != "Hello, BetterOCR!" {
		t.Errorf("final.Text = %q, want consensus text", final.Text)
	}
	if final.Votes["a"] != 1 || final.Votes["b"] != 1 || final.Votes["c"] != 0 {
		t.Errorf("unexpected votes: %v", final.Votes)
	}
}

func TestFuseAllFailed(t *testing.T) {
	a := arbiterNew()
	final := a.Fuse([]agent.Result{
		{Agent: "a", Err: "boom"},
		{Agent: "b", Text: "", Confidence: 0.9},
	})
	if final.Text != "" {
		t.Errorf("expected empty final on all-invalid inputs, got %q", final.Text)
	}
}

func arbiterNew() *Arbiter { return New() }
