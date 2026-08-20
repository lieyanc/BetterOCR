package arbiter

import (
	"reflect"
	"testing"

	"github.com/lieyanc/BetterOCR/internal/agent"
)

func full(name, text string) agent.Result {
	return agent.Result{Agent: name, Text: text}
}

func rowTexts(rows []*row) [][]string {
	out := make([][]string, len(rows))
	for i, row := range rows {
		for _, candidate := range row.cands {
			out[i] = append(out[i], candidate.text)
		}
	}
	return out
}

func TestAlignIgnoresDifferentPhysicalLines(t *testing.T) {
	rows := alignAll([]agent.Result{
		full("a", "第一句跨了\n两行。第二句。"),
		full("b", "第一句跨了两行。\n第二句。"),
	}, 0.35)
	want := [][]string{{"第一句跨了两行。", "第一句跨了两行。"}, {"第二句。", "第二句。"}}
	if got := rowTexts(rows); !reflect.DeepEqual(got, want) {
		t.Fatalf("rows = %#v, want %#v", got, want)
	}
}

func TestAlignAbsorbsSentenceBoundaryDifference(t *testing.T) {
	rows := alignAll([]agent.Result{
		full("a", "第一句。第二句。"),
		full("b", "第一句第二句。"),
	}, 0.35)
	if len(rows) != 1 || len(rows[0].cands) != 2 {
		t.Fatalf("boundary mismatch should become one dispute: %#v", rowTexts(rows))
	}
	if got := rowTexts(rows)[0]; !reflect.DeepEqual(got, []string{"第一句。第二句。", "第一句第二句。"}) {
		t.Fatalf("candidates = %#v", got)
	}
}

func TestAlignMissingSentenceAndDeterministic(t *testing.T) {
	results := []agent.Result{
		full("a", "甲句内容。乙句内容。丙句内容。"),
		full("b", "甲句内容。丙句内容。"),
	}
	want := [][]string{{"甲句内容。", "甲句内容。"}, {"乙句内容。"}, {"丙句内容。", "丙句内容。"}}
	for i := 0; i < 30; i++ {
		if got := rowTexts(alignAll(results, 0.35)); !reflect.DeepEqual(got, want) {
			t.Fatalf("run %d rows = %#v, want %#v", i, got, want)
		}
	}
}

func TestAlignRequiresSimilarityStrictlyAboveThreshold(t *testing.T) {
	rows := alignAll([]agent.Result{
		full("a", "相同句段。"),
		full("b", "相同句段。"),
	}, 1)
	if len(rows) != 2 {
		t.Fatalf("similarity equal to threshold must not align: %#v", rowTexts(rows))
	}
}

func TestRowRepPicksMedoidDeterministically(t *testing.T) {
	row := &row{cands: []cand{
		{agent: "c", text: "完全无关内容。"},
		{agent: "b", text: "这是正确内容。"},
		{agent: "a", text: "这是正确内容。"},
	}}
	if got := row.rep(); got.agent != "a" || got.text != "这是正确内容。" {
		t.Fatalf("rep = %+v", got)
	}
}
