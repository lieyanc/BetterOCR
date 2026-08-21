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

func TestAlignAbsorbsLongSentenceBoundaryDifference(t *testing.T) {
	rows := alignAll([]agent.Result{
		full("a", "前锚点。第一部分。第二部分。第三部分。第四部分。第五部分。后锚点。"),
		full("b", "前锚点。第一部分第二部分第三部分第四部分第五部分。后锚点。"),
		full("c", "前锚点。第一部分第二部分。第三部分第四部分第五部分。后锚点。"),
	}, 0.35)
	if len(rows) != 3 {
		t.Fatalf("long boundary mismatch should become one middle slot: %#v", rowTexts(rows))
	}
	if got := len(rows[1].cands); got != 3 {
		t.Fatalf("middle candidate count = %d, want 3: %#v", got, rowTexts(rows))
	}
}

func TestAlignSupportsManyToManyBoundaryShift(t *testing.T) {
	rows := alignAll([]agent.Result{
		full("a", "前锚点。甲乙。丙丁。戊己。后锚点。"),
		full("b", "前锚点。甲。乙丙。丁戊。己。后锚点。"),
	}, 0.35)
	want := [][]string{
		{"前锚点。", "前锚点。"},
		{"甲乙。丙丁。戊己。", "甲。乙丙。丁戊。己。"},
		{"后锚点。", "后锚点。"},
	}
	if got := rowTexts(rows); !reflect.DeepEqual(got, want) {
		t.Fatalf("rows = %#v, want %#v", got, want)
	}
}

func TestAlignPreservesRealRepeatedSentences(t *testing.T) {
	rows := alignAll([]agent.Result{
		full("a", "开头。重复内容。重复内容。结尾。"),
		full("b", "开头。重复内容。重复内容。结尾。"),
	}, 0.35)
	if len(rows) != 4 {
		t.Fatalf("real repeated sentences were collapsed: %#v", rowTexts(rows))
	}
	for i, row := range rows {
		if len(row.cands) != 2 {
			t.Fatalf("row %d candidates = %#v, want both engines", i, rowTexts(rows))
		}
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
