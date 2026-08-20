package arbiter

import (
	"reflect"
	"testing"
)

func TestNormalizeKeepsVisibleSymbols(t *testing.T) {
	if got := Normalize(" 金额： ¥ 1,280.00 \n"); got != "金额：¥1,280.00" {
		t.Fatalf("Normalize = %q", got)
	}
	if Normalize("金额：¥128") == Normalize("金额：128") {
		t.Fatal("visible currency symbol must remain an OCR dispute")
	}
	if CoreNormalize("金额：¥ 128。") != CoreNormalize("金额 128") {
		t.Fatal("symbol-free core should only be used to locate corresponding text")
	}
}

func TestSimilarityChinese(t *testing.T) {
	if got := Similarity("本次应付金额：128元。", "本次应付金额 128元"); got != 1 {
		t.Fatalf("symbol-only difference similarity = %v, want 1", got)
	}
	got := Similarity("多引擎融合", "多引擎融台")
	if got <= 0.5 || got >= 1 {
		t.Fatalf("one-character difference = %v, want in (0.5,1)", got)
	}
}

func TestSplitSegmentsIgnoresPhysicalLines(t *testing.T) {
	in := "这是第一\n句话。 \n这是第二句话！“第三句？\n”末尾无标点"
	want := []string{"这是第一句话。", "这是第二句话！", "“第三句？”", "末尾无标点"}
	if got := SplitSegments(in); !reflect.DeepEqual(got, want) {
		t.Fatalf("SplitSegments = %#v, want %#v", got, want)
	}
}

func TestSplitSegmentsPreservesEnglishWordSpace(t *testing.T) {
	if got := SplitSegments("Better\nOCR works!"); !reflect.DeepEqual(got, []string{"Better OCR works!"}) {
		t.Fatalf("SplitSegments = %#v", got)
	}
}
