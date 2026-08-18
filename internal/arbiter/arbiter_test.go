package arbiter

import (
	"context"
	"errors"
	"math"
	"reflect"
	"testing"

	"github.com/lieyanc/BetterOCR/internal/agent"
)

func res(name string, lines ...string) agent.Result {
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
// 严格多数走共识;并验证分歧上下文、置信度推导与统计。
func TestFuseThreePaths(t *testing.T) {
	esc := &fakeEsc{fn: func(ds []Dispute) ([]Resolution, error) {
		return []Resolution{{Row: 0, Text: "Hello, BetterOCR!"}}, nil
	}}
	arb := New()
	arb.Escalator = esc

	image := []byte{1, 2, 3}
	// 故意打乱输入顺序,Fuse 应按引擎名重排
	final := arb.Fuse(context.Background(), image, []agent.Result{
		res("c", "He110, BetterOCR!", "多引擎 OCR 融合示例", "consensus beats conf1dence"),
		res("a", "Hel1o, BetterOCR!", "多引擎 OCR 融合示例", "consensus beats confidence"),
		res("b", "Hello, BetterOCR!", "多引擎 OCR 融合示例", "consensus beats confidence"),
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
		{Agent: "a", Text: "Hel1o, BetterOCR!"},
		{Agent: "b", Text: "Hello, BetterOCR!"},
		{Agent: "c", Text: "He110, BetterOCR!"},
	}
	if !reflect.DeepEqual(d.Candidates, wantCands) {
		t.Errorf("dispute candidates = %+v, want %+v", d.Candidates, wantCands)
	}

	wantLines := []FinalLine{
		// 仲裁裁定与引擎 b 的候选逐字吻合 → 拿到独立旁证
		{Text: "Hello, BetterOCR!", Confidence: escalatedCorroborated, Source: SourceEscalated, From: []string{"fake-arbiter"}},
		// 三家逐字一致
		{Text: "多引擎 OCR 融合示例", Confidence: round4(consensusConfidence(3, 3)), Source: SourceConsensus, From: []string{"a", "b", "c"}},
		// 三家中两家一致:是共识,但异议存在,置信度明显低于上一行
		{Text: "consensus beats confidence", Confidence: round4(consensusConfidence(2, 3)), Source: SourceConsensus, From: []string{"a", "b"}},
	}
	if !reflect.DeepEqual(final.Lines, wantLines) {
		t.Errorf("lines = %+v, want %+v", final.Lines, wantLines)
	}
	if want := "Hello, BetterOCR!\n多引擎 OCR 融合示例\nconsensus beats confidence"; final.Text != want {
		t.Errorf("text = %q, want %q", final.Text, want)
	}
	wantConf := round4((wantLines[0].Confidence + wantLines[1].Confidence + wantLines[2].Confidence) / 3)
	if final.Confidence != wantConf {
		t.Errorf("confidence = %v, want %v", final.Confidence, wantConf)
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

// TestConsensusNeedsMajorityOfAllEngines 锁定共识判据的分母:是全体有效
// 引擎数,不是该行槽上的候选数。某个引擎在这一行没有产出候选,本身就是
// "这行可能不存在"的证据。旧判据下 4 个引擎里 2 个看到并一致就直接通过。
func TestConsensusNeedsMajorityOfAllEngines(t *testing.T) {
	shared, lone := "shared line here", "only some engines see this"

	// 4 个引擎,其中只有 a、b 产出了 lone 行 → 2/4 不构成严格多数 → 分歧
	esc := &fakeEsc{}
	arb := New()
	arb.Escalator = esc
	final := arb.Fuse(context.Background(), nil, []agent.Result{
		res("a", shared, lone), res("b", shared, lone),
		res("c", shared), res("d", shared),
	})
	if final.Stats.ConsensusRows != 1 || final.Stats.Rows != 2 {
		t.Errorf("stats = %+v, want 2 rows / only the shared row as consensus", final.Stats)
	}
	if len(esc.calls) != 1 || len(esc.calls[0]) != 1 || esc.calls[0][0].Row != 1 {
		t.Fatalf("escalator calls = %+v, want the 2-of-4 row escalated", esc.calls)
	}

	// 同样是 2 家一致,但总共只有 3 个引擎 → 2/3 是严格多数 → 共识
	esc3 := &fakeEsc{}
	arb3 := New()
	arb3.Escalator = esc3
	final3 := arb3.Fuse(context.Background(), nil, []agent.Result{
		res("a", shared, lone), res("b", shared, lone), res("c", shared),
	})
	if final3.Stats.ConsensusRows != 2 || len(esc3.calls) != 0 {
		t.Errorf("stats = %+v, escalations = %d; want both rows consensus, no escalation",
			final3.Stats, len(esc3.calls))
	}
}

// TestConfidenceModel 锁定置信度刻度本身:它必须随一致引擎数上升、
// 随异议下降,并且三条路径落在互不重叠、与界面色带对应的区间里。
func TestConfidenceModel(t *testing.T) {
	// 逐字一致的引擎越多越可信
	if !(consensusConfidence(2, 2) < consensusConfidence(3, 3) &&
		consensusConfidence(3, 3) < consensusConfidence(5, 5)) {
		t.Error("consensus confidence must increase with the number of agreeing engines")
	}
	// 同样 k 家一致,存在异议时必须更低
	if !(consensusConfidence(2, 3) < consensusConfidence(2, 2) &&
		consensusConfidence(3, 5) < consensusConfidence(3, 4) &&
		consensusConfidence(3, 4) < consensusConfidence(3, 3)) {
		t.Error("dissent must lower consensus confidence")
	}
	// 全体一致落在界面绿色档(≥0.9),存在异议落进黄色档
	if consensusConfidence(2, 2) < 0.9 || consensusConfidence(3, 3) < 0.9 {
		t.Errorf("unanimous rows should read as high confidence: 2/2=%v 3/3=%v",
			consensusConfidence(2, 2), consensusConfidence(3, 3))
	}
	if c := consensusConfidence(2, 3); c < 0.7 || c >= 0.9 {
		t.Errorf("2-of-3 consensus = %v, want the amber band [0.7, 0.9)", c)
	}
	// 兜底行永远不该看起来可信:整个区间都在绿色档之下
	if fallbackCeil >= 0.7 || fallbackLone > fallbackFloor {
		t.Errorf("fallback band [%v, %v] (lone %v) must stay below the amber cutoff",
			fallbackFloor, fallbackCeil, fallbackLone)
	}
	// 有旁证的仲裁结果必须高于无旁证的
	if escalatedSolo >= escalatedCorroborated {
		t.Error("corroborated escalation must outrank a solo one")
	}
}

// TestFallbackConfidenceTracksAgreement 验证兜底置信度随候选的接近程度变化:
// 只差一个字符时接近上限,面目全非时接近下限,孤行最低。
func TestFallbackConfidenceTracksAgreement(t *testing.T) {
	near := &row{cands: []cand{{agent: "a", text: "alpha bravo charlie"}, {agent: "b", text: "alpha bravo charl1e"}}}
	far := &row{cands: []cand{{agent: "a", text: "alpha bravo charlie"}, {agent: "b", text: "xxxxx yyyyy zzzzz"}}}
	lone := &row{cands: []cand{{agent: "a", text: "alpha bravo charlie"}}}

	nearConf := fallbackConfidence(near, near.rep())
	farConf := fallbackConfidence(far, far.rep())
	loneConf := fallbackConfidence(lone, lone.rep())

	if !(loneConf < farConf && farConf < nearConf && nearConf <= fallbackCeil) {
		t.Errorf("fallback confidences lone=%v far=%v near=%v, want lone < far < near <= %v",
			loneConf, farConf, nearConf, fallbackCeil)
	}
	if farConf < fallbackFloor {
		t.Errorf("far = %v, want >= floor %v", farConf, fallbackFloor)
	}
}

// TestEscalatedConfidenceCorroboration 验证仲裁结果是否被独立引擎旁证
// 会改变置信度——这是仲裁行上唯一可观测的区分信号。
func TestEscalatedConfidenceCorroboration(t *testing.T) {
	r := &row{cands: []cand{{agent: "a", text: "invoice  042"}, {agent: "b", text: "inv0ice 042"}}}
	// 排版空白不构成分歧,规整后逐字相同即算旁证
	if got := escalatedConfidence(r, "invoice 042"); got != escalatedCorroborated {
		t.Errorf("corroborated = %v, want %v", got, escalatedCorroborated)
	}
	if got := escalatedConfidence(r, "invoice 843"); got != escalatedSolo {
		t.Errorf("solo = %v, want %v", got, escalatedSolo)
	}
}

// TestFuseTieDeterministic 是并列结果确定性的回归测试:
// 旧实现依赖 map 迭代顺序,同一输入可产出不同结果。
func TestFuseTieDeterministic(t *testing.T) {
	results := []agent.Result{res("a", "alpha beta"), res("b", "GAMMA DELTA")}
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

// TestFuseSingleEngine 是 0/0=NaN 的回归测试。单引擎没有任何旁证,
// 每行都是孤行,置信度应当停在最低档。
func TestFuseSingleEngine(t *testing.T) {
	final := New().Fuse(context.Background(), nil, []agent.Result{
		res("solo", "line one", "line two"),
	})
	if math.IsNaN(final.Confidence) {
		t.Fatal("confidence is NaN")
	}
	if final.Confidence != fallbackLone {
		t.Errorf("confidence = %v, want %v", final.Confidence, fallbackLone)
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
		res("a", "alpha alpha"), res("b", "alpha a1pha"),
	})
	if final.Stats.EscalationErr != "model exploded" {
		t.Errorf("EscalationErr = %q, want model exploded", final.Stats.EscalationErr)
	}
	if final.Stats.EscalatedRows != 0 || final.Stats.FallbackRows != 1 {
		t.Errorf("stats = %+v, want 0 escalated / 1 fallback", final.Stats)
	}
	if len(final.Lines) != 1 || final.Lines[0].Text != "alpha alpha" || final.Lines[0].Source != SourceFallback {
		t.Fatalf("lines = %+v, want fallback to the medoid candidate", final.Lines)
	}
	if c := final.Lines[0].Confidence; c < fallbackFloor || c > fallbackCeil {
		t.Errorf("fallback confidence = %v, want within [%v, %v]", c, fallbackFloor, fallbackCeil)
	}
}

// TestFuseEscalatorPartialAndDrop 验证:空文本裁定丢行、漏答的行本地兜底、
// 不在分歧集合中的行号被忽略。
func TestFuseEscalatorPartialAndDrop(t *testing.T) {
	esc := &fakeEsc{fn: func([]Dispute) ([]Resolution, error) {
		return []Resolution{
			{Row: 0, Text: "alpha alpha"},
			{Row: 1, Text: "   "},    // 空白 = 图中不存在,丢弃
			{Row: 99, Text: "ghost"}, // 幻觉行号,应被忽略
		}, nil
	}}
	arb := New()
	arb.Escalator = esc
	final := arb.Fuse(context.Background(), nil, []agent.Result{
		res("a", "alpha alpha", "bravo bravo", "charlie charlie"),
		res("b", "alpha a1pha", "bravo brav0", "charlie charl1e"),
	})

	if len(final.Lines) != 2 {
		t.Fatalf("lines = %+v, want 2", final.Lines)
	}
	if l := final.Lines[0]; l.Text != "alpha alpha" || l.Source != SourceEscalated ||
		l.Confidence != escalatedCorroborated || !reflect.DeepEqual(l.From, []string{"fake-arbiter"}) {
		t.Errorf("lines[0] = %+v, want corroborated escalation", l)
	}
	if l := final.Lines[1]; l.Text != "charlie charlie" || l.Source != SourceFallback ||
		!reflect.DeepEqual(l.From, []string{"a"}) || l.Confidence > fallbackCeil {
		t.Errorf("lines[1] = %+v, want fallback from a", l)
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
		res("a", "hello world"),
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
