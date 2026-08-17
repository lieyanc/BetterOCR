package arbiter

import (
	"context"
	"errors"
	"math"
	"reflect"
	"testing"

	"github.com/lieyanc/BetterOCR/internal/agent"
)

func ln(text string, conf float64) agent.Line {
	return agent.Line{Text: text, Confidence: conf}
}

func res(name string, lines ...agent.Line) agent.Result {
	return agent.Result{Agent: name, Lines: lines}
}

// fakeEsc 是仅测试用的 Escalator 假实现。
type fakeEsc struct {
	name     string
	fn       func([]Dispute) ([]Resolution, error)
	calls    [][]Dispute
	gotImage []byte
}

func (f *fakeEsc) Name() string {
	if f.name == "" {
		return "fake-arbiter"
	}
	return f.name
}

func (f *fakeEsc) Resolve(_ context.Context, image []byte, ds []Dispute) ([]Resolution, error) {
	f.gotImage = image
	f.calls = append(f.calls, ds)
	if f.fn == nil {
		return nil, nil
	}
	return f.fn(ds)
}

// TestFuseThreePaths 覆盖三条产出路径:分歧行走仲裁、全体一致走共识、
// 严格多数走共识;并验证分歧上下文、置信度合成与统计。
func TestFuseThreePaths(t *testing.T) {
	esc := &fakeEsc{fn: func(ds []Dispute) ([]Resolution, error) {
		return []Resolution{{Row: 0, Text: "Hello, BetterOCR!", Confidence: 0.98}}, nil
	}}
	arb := New()
	arb.Escalator = esc

	image := []byte{1, 2, 3}
	// 故意打乱输入顺序,Fuse 应按引擎名重排
	final := arb.Fuse(context.Background(), image, []agent.Result{
		res("c", ln("He110, BetterOCR!", 0.85), ln("多引擎 OCR 融合示例", 0.80), ln("consensus beats conf1dence", 0.93)),
		res("a", ln("Hel1o, BetterOCR!", 0.90), ln("多引擎 OCR 融合示例", 0.92), ln("consensus beats confidence", 0.88)),
		res("b", ln("Hello, BetterOCR!", 0.95), ln("多引擎 OCR 融合示例", 0.90), ln("consensus beats confidence", 0.86)),
	})

	if !reflect.DeepEqual(esc.gotImage, image) {
		t.Errorf("escalator did not receive original image")
	}
	if len(esc.calls) != 1 || len(esc.calls[0]) != 1 {
		t.Fatalf("escalator calls = %v, want exactly 1 call with 1 dispute", esc.calls)
	}
	d := esc.calls[0][0]
	if d.Row != 0 || d.Before != "" || d.After != "多引擎 OCR 融合示例" {
		t.Errorf("dispute context = %+v, want Row 0, no Before, After consensus line", d)
	}
	wantCands := []Candidate{
		{Agent: "a", Text: "Hel1o, BetterOCR!", Confidence: 0.90},
		{Agent: "b", Text: "Hello, BetterOCR!", Confidence: 0.95},
		{Agent: "c", Text: "He110, BetterOCR!", Confidence: 0.85},
	}
	if !reflect.DeepEqual(d.Candidates, wantCands) {
		t.Errorf("dispute candidates = %+v, want %+v", d.Candidates, wantCands)
	}

	wantLines := []FinalLine{
		{Text: "Hello, BetterOCR!", Confidence: 0.98, Source: SourceEscalated, From: []string{"fake-arbiter"}},
		// 三家一致:1-(0.08*0.10*0.20) = 0.9984,一致性本身是证据
		{Text: "多引擎 OCR 融合示例", Confidence: 0.9984, Source: SourceConsensus, From: []string{"a", "b", "c"}},
		// 两家多数:1-(0.12*0.14) = 0.9832,压过 c 更高的单点置信度 0.93
		{Text: "consensus beats confidence", Confidence: 0.9832, Source: SourceConsensus, From: []string{"a", "b"}},
	}
	if !reflect.DeepEqual(final.Lines, wantLines) {
		t.Errorf("lines = %+v, want %+v", final.Lines, wantLines)
	}
	if want := "Hello, BetterOCR!\n多引擎 OCR 融合示例\nconsensus beats confidence"; final.Text != want {
		t.Errorf("text = %q, want %q", final.Text, want)
	}
	if final.Confidence != 0.9872 {
		t.Errorf("confidence = %v, want 0.9872", final.Confidence)
	}

	wantStats := Stats{Engines: 3, Rows: 3, ConsensusRows: 2, EscalatedRows: 1, Escalator: "fake-arbiter"}
	if final.Stats != wantStats {
		t.Errorf("stats = %+v, want %+v", final.Stats, wantStats)
	}
	for i, want := range []string{"a", "b", "c"} {
		if final.Candidates[i].Agent != want {
			t.Errorf("candidates[%d].Agent = %q, want %q", i, final.Candidates[i].Agent, want)
		}
	}
}

// TestFuseTieDeterministic 是并列结果确定性的回归测试:
// 旧实现依赖 map 迭代顺序,同一输入可产出不同结果。
func TestFuseTieDeterministic(t *testing.T) {
	results := []agent.Result{
		res("a", ln("alpha beta", 0.9)),
		res("b", ln("GAMMA DELTA", 0.9)),
	}
	first := New().Fuse(context.Background(), nil, results)
	if len(first.Lines) != 2 {
		t.Fatalf("lines = %+v, want 2 fallback lines", first.Lines)
	}
	for i := 0; i < 100; i++ {
		got := New().Fuse(context.Background(), nil, results)
		if !reflect.DeepEqual(got, first) {
			t.Fatalf("run %d differs:\n%+v\nvs\n%+v", i, got, first)
		}
	}
}

// TestFuseSingleEngine 是 0/0=NaN 的回归测试。
func TestFuseSingleEngine(t *testing.T) {
	final := New().Fuse(context.Background(), nil, []agent.Result{
		res("solo", ln("line one", 0.8), ln("line two", 0.6)),
	})
	if math.IsNaN(final.Confidence) {
		t.Fatal("confidence is NaN")
	}
	if final.Confidence != 0.7 {
		t.Errorf("confidence = %v, want 0.7", final.Confidence)
	}
	if final.Stats.FallbackRows != 2 || final.Stats.ConsensusRows != 0 {
		t.Errorf("stats = %+v, want 2 fallback rows", final.Stats)
	}
	for _, l := range final.Lines {
		if l.Source != SourceFallback || !reflect.DeepEqual(l.From, []string{"solo"}) {
			t.Errorf("line = %+v, want fallback from solo", l)
		}
	}
}

// TestFuseEscalatorError 验证仲裁失败不拖垮整体,分歧行本地兜底。
func TestFuseEscalatorError(t *testing.T) {
	esc := &fakeEsc{fn: func([]Dispute) ([]Resolution, error) {
		return nil, errors.New("model exploded")
	}}
	arb := New()
	arb.Escalator = esc
	final := arb.Fuse(context.Background(), nil, []agent.Result{
		res("a", ln("alpha alpha", 0.9)),
		res("b", ln("alpha a1pha", 0.85)),
	})
	if final.Stats.EscalationErr != "model exploded" {
		t.Errorf("EscalationErr = %q, want model exploded", final.Stats.EscalationErr)
	}
	if final.Stats.EscalatedRows != 0 || final.Stats.FallbackRows != 1 {
		t.Errorf("stats = %+v, want 0 escalated / 1 fallback", final.Stats)
	}
	if len(final.Lines) != 1 || final.Lines[0].Text != "alpha alpha" || final.Lines[0].Source != SourceFallback {
		t.Errorf("lines = %+v, want fallback to highest-confidence candidate", final.Lines)
	}
}

// TestFuseEscalatorPartialAndDrop 验证:空文本裁定丢行、漏答的行本地兜底、
// 不在分歧集合中的行号被忽略。
func TestFuseEscalatorPartialAndDrop(t *testing.T) {
	esc := &fakeEsc{fn: func([]Dispute) ([]Resolution, error) {
		return []Resolution{
			{Row: 0, Text: "alpha alpha", Confidence: 0.99},
			{Row: 1, Text: "   ", Confidence: 0.5},  // 空白 = 图中不存在,丢弃
			{Row: 99, Text: "ghost", Confidence: 1}, // 幻觉行号,应被忽略
		}, nil
	}}
	arb := New()
	arb.Escalator = esc
	final := arb.Fuse(context.Background(), nil, []agent.Result{
		res("a", ln("alpha alpha", 0.9), ln("bravo bravo", 0.8), ln("charlie charlie", 0.7)),
		res("b", ln("alpha a1pha", 0.85), ln("bravo brav0", 0.75), ln("charlie charl1e", 0.65)),
	})

	wantLines := []FinalLine{
		{Text: "alpha alpha", Confidence: 0.99, Source: SourceEscalated, From: []string{"fake-arbiter"}},
		{Text: "charlie charlie", Confidence: 0.7, Source: SourceFallback, From: []string{"a"}},
	}
	if !reflect.DeepEqual(final.Lines, wantLines) {
		t.Errorf("lines = %+v, want %+v", final.Lines, wantLines)
	}
	if final.Text != "alpha alpha\ncharlie charlie" {
		t.Errorf("text = %q", final.Text)
	}
	s := final.Stats
	if s.Rows != 3 || s.EscalatedRows != 2 || s.DroppedRows != 1 || s.FallbackRows != 1 || s.ConsensusRows != 0 {
		t.Errorf("stats = %+v, want rows 3 / escalated 2 / dropped 1 / fallback 1", s)
	}
}

// TestFuseSkipsFailedAndEmpty 验证失败引擎与空结果不参与融合但保留在 Candidates。
func TestFuseSkipsFailedAndEmpty(t *testing.T) {
	final := New().Fuse(context.Background(), nil, []agent.Result{
		{Agent: "c", Err: "boom"},
		res("a", ln("hello world", 0.9)),
		{Agent: "b"}, // 成功但零行
	})
	if final.Stats.Engines != 3 || final.Stats.FailedEngines != 1 || final.Stats.Rows != 1 {
		t.Errorf("stats = %+v, want engines 3 / failed 1 / rows 1", final.Stats)
	}
	if len(final.Lines) != 1 || final.Lines[0].Text != "hello world" {
		t.Errorf("lines = %+v", final.Lines)
	}
	for i, want := range []string{"a", "b", "c"} {
		if final.Candidates[i].Agent != want {
			t.Errorf("candidates[%d] = %q, want %q (name-sorted)", i, final.Candidates[i].Agent, want)
		}
	}
}

// TestFuseNoValidResults 验证全失败与空输入的边界。
func TestFuseNoValidResults(t *testing.T) {
	final := New().Fuse(context.Background(), nil, []agent.Result{
		{Agent: "b", Err: "x"},
		{Agent: "a", Err: "y"},
	})
	if final.Text != "" || len(final.Lines) != 0 || final.Lines == nil {
		t.Errorf("final = %+v, want empty non-nil lines", final)
	}
	if final.Stats.FailedEngines != 2 || final.Confidence != 0 {
		t.Errorf("stats = %+v conf %v", final.Stats, final.Confidence)
	}

	empty := New().Fuse(context.Background(), nil, nil)
	if len(empty.Lines) != 0 || empty.Stats.Engines != 0 {
		t.Errorf("empty input: %+v", empty)
	}
}
