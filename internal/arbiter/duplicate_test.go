package arbiter

import "testing"

func TestApplyDuplicatePairsRemovesOnlyValidatedLaterDispute(t *testing.T) {
	final := Final{
		Text:       "可靠开头文字。\n合同金额人民币一百二十元。\n合同金额人民币一百二十元。\n可靠结尾文字。",
		Confidence: 0.85,
		Segments: []FinalSegment{
			{Text: "可靠开头文字。", Confidence: 0.9, Source: SourceConsensus},
			{Text: "合同金额人民币一百二十元。", Confidence: 0.95, Source: SourceConsensus},
			{Text: "合同金额人民币一百二十元。", Confidence: 0.6, Source: SourceEscalated, Disputed: true},
			{Text: "可靠结尾文字。", Confidence: 0.95, Source: SourceConsensus},
		},
	}

	got, removed := ApplyDuplicatePairs(final, []DuplicatePair{{Later: 2, Earlier: 1}})
	if removed != 1 || len(got.Segments) != 3 {
		t.Fatalf("removed=%d segments=%d, want 1 and 3", removed, len(got.Segments))
	}
	if got.Text != "可靠开头文字。\n合同金额人民币一百二十元。\n可靠结尾文字。" {
		t.Fatalf("text = %q", got.Text)
	}
	if got.Confidence != 0.9333 {
		t.Fatalf("confidence = %v, want 0.9333", got.Confidence)
	}
	if len(final.Segments) != 4 {
		t.Fatal("ApplyDuplicatePairs mutated the input segment slice")
	}
}

func TestApplyDuplicatePairsAcceptsSmallOCRVariationAndContainment(t *testing.T) {
	tests := map[string]struct{ earlier, later string }{
		"small OCR variation": {"统一社会信用代码91310000ABCDEFG。", "统一社会信用代码91310000ABCPEFG。"},
		"later contained":     {"重要提示：请仔细阅读以下全部合同条款。", "请仔细阅读以下全部合同条款。"},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			final := Final{Segments: []FinalSegment{
				{Text: test.earlier, Source: SourceConsensus},
				{Text: test.later, Source: SourceFallback, Disputed: true},
			}}
			got, removed := ApplyDuplicatePairs(final, []DuplicatePair{{Later: 1, Earlier: 0}})
			if removed != 1 || len(got.Segments) != 1 {
				t.Fatalf("removed=%d segments=%d", removed, len(got.Segments))
			}
		})
	}
}

func TestApplyDuplicatePairsPreservesUnsafeProposals(t *testing.T) {
	base := []FinalSegment{
		{Text: "合同金额人民币一百二十元。", Source: SourceConsensus},
		{Text: "合同金额人民币一百二十元。", Source: SourceConsensus},
		{Text: "短句重复。", Source: SourceFallback, Disputed: true},
		{Text: "过渡区域的其他有效正文。", Source: SourceConsensus},
		{Text: "另一段完全不同并且必须保留的正文。", Source: SourceFallback, Disputed: true},
		{Text: "占位一。", Source: SourceConsensus},
		{Text: "合同金额人民币一百二十元。", Source: SourceFallback, Disputed: true},
	}
	tests := map[string]DuplicatePair{
		"later consensus": {Later: 1, Earlier: 0},
		"too short":       {Later: 2, Earlier: 0},
		"not similar":     {Later: 4, Earlier: 3},
		"too far":         {Later: 6, Earlier: 0},
		"reverse":         {Later: 0, Earlier: 1},
		"out of range":    {Later: 99, Earlier: 0},
	}
	for name, pair := range tests {
		t.Run(name, func(t *testing.T) {
			final := Final{Segments: append([]FinalSegment(nil), base...)}
			got, removed := ApplyDuplicatePairs(final, []DuplicatePair{pair})
			if removed != 0 || len(got.Segments) != len(base) {
				t.Fatalf("removed=%d segments=%d", removed, len(got.Segments))
			}
		})
	}
}
