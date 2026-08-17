package arbiter

import (
	"reflect"
	"testing"

	"github.com/lieyanc/BetterOCR/internal/agent"
)

func mkres(name string, texts ...string) agent.Result {
	lines := make([]agent.Line, len(texts))
	for i, s := range texts {
		lines[i] = agent.Line{Text: s, Confidence: 0.9}
	}
	return agent.Result{Agent: name, Lines: lines}
}

func rowTexts(rows []*row) [][]string {
	out := make([][]string, len(rows))
	for i, r := range rows {
		for _, c := range r.cands {
			out[i] = append(out[i], c.line.Text)
		}
	}
	return out
}

func TestAlignIdenticalLines(t *testing.T) {
	rows := alignAll([]agent.Result{
		mkres("a", "alpha one", "bravo two"),
		mkres("b", "alpha one", "bravo two"),
	}, 0.35)
	want := [][]string{
		{"alpha one", "alpha one"},
		{"bravo two", "bravo two"},
	}
	if got := rowTexts(rows); !reflect.DeepEqual(got, want) {
		t.Errorf("rows = %v, want %v", got, want)
	}
}

func TestAlignMissingLine(t *testing.T) {
	// b 漏识别了中间一行,其余行仍应两两对齐
	rows := alignAll([]agent.Result{
		mkres("a", "alpha one", "bravo two", "charlie three"),
		mkres("b", "alpha one", "charlie three"),
	}, 0.35)
	want := [][]string{
		{"alpha one", "alpha one"},
		{"bravo two"},
		{"charlie three", "charlie three"},
	}
	if got := rowTexts(rows); !reflect.DeepEqual(got, want) {
		t.Errorf("rows = %v, want %v", got, want)
	}
}

func TestAlignInsertedLine(t *testing.T) {
	// b 多识别出一行,应按阅读顺序插入新行槽
	rows := alignAll([]agent.Result{
		mkres("a", "alpha one", "charlie three"),
		mkres("b", "alpha one", "bravo two", "charlie three"),
	}, 0.35)
	want := [][]string{
		{"alpha one", "alpha one"},
		{"bravo two"},
		{"charlie three", "charlie three"},
	}
	if got := rowTexts(rows); !reflect.DeepEqual(got, want) {
		t.Errorf("rows = %v, want %v", got, want)
	}
}

func TestAlignFuzzyMatchWithinThreshold(t *testing.T) {
	// 一两个字符的识别差异不应拆成两行
	rows := alignAll([]agent.Result{
		mkres("a", "Hello, BetterOCR!"),
		mkres("b", "He110, BetterOCR!"),
	}, 0.35)
	if len(rows) != 1 || len(rows[0].cands) != 2 {
		t.Errorf("similar lines should share one row, got %v", rowTexts(rows))
	}
}

func TestAlignDissimilarSeparateAndDeterministic(t *testing.T) {
	results := []agent.Result{mkres("a", "xxxx yyyy"), mkres("b", "zzzz wwww")}
	first := rowTexts(alignAll(results, 0.35))
	if len(first) != 2 {
		t.Fatalf("dissimilar lines must not merge, got %v", first)
	}
	for i := 0; i < 50; i++ {
		if got := rowTexts(alignAll(results, 0.35)); !reflect.DeepEqual(got, first) {
			t.Fatalf("alignment nondeterministic: %v vs %v", got, first)
		}
	}
}

func TestRowRepDeterministic(t *testing.T) {
	r := &row{cands: []cand{
		{agent: "b", line: agent.Line{Text: "x", Confidence: 0.9}},
		{agent: "a", line: agent.Line{Text: "y", Confidence: 0.9}},
	}}
	// 平局按引擎名字典序
	if got := r.rep(); got.agent != "a" {
		t.Errorf("rep tie-break = %q, want %q", got.agent, "a")
	}
}
