package arbiter

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/lieyanc/BetterOCR/internal/agent"
)

func result(name, text string) agent.Result {
	return agent.Result{Agent: name, Text: text}
}

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

func (f *fakeEsc) Resolve(_ context.Context, image []byte, disputes []Dispute) ([]Resolution, error) {
	f.gotImage = image
	f.calls = append(f.calls, disputes)
	if f.fn == nil {
		return nil, nil
	}
	return f.fn(disputes)
}

func TestFuseChineseSegmentsThreePaths(t *testing.T) {
	esc := &fakeEsc{fn: func(disputes []Dispute) ([]Resolution, error) {
		return []Resolution{{Segment: disputes[0].Segment, Text: "发票号码042。"}}, nil
	}}
	fuser := New()
	fuser.Escalator = esc
	image := []byte{1, 2, 3}
	final := fuser.Fuse(context.Background(), image, []agent.Result{
		result("c", "发票号码0A2。\n总金额¥128。付款状态为未支付。"),
		result("a", "发票号码042。总金额 ¥128。付款状态为已支付。"),
		result("b", "发票号码O42。总金额¥128。付款状态为已支付。"),
	})

	if !reflect.DeepEqual(esc.gotImage, image) || len(esc.calls) != 1 || len(esc.calls[0]) != 1 {
		t.Fatalf("escalation = calls %#v image %v", esc.calls, esc.gotImage)
	}
	dispute := esc.calls[0][0]
	if dispute.Segment != 0 || dispute.After != "总金额¥128。" || len(dispute.Candidates) != 3 {
		t.Fatalf("dispute = %+v", dispute)
	}
	if len(final.Segments) != 3 {
		t.Fatalf("segments = %+v", final.Segments)
	}
	if segment := final.Segments[0]; segment.Source != SourceEscalated || !segment.Disputed ||
		segment.Text != "发票号码042。" || segment.Confidence != escalatedCorroborated || len(segment.Candidates) != 3 {
		t.Fatalf("escalated segment = %+v", segment)
	}
	if final.Segments[1].Source != SourceConsensus || final.Segments[2].Source != SourceConsensus {
		t.Fatalf("consensus segments = %+v", final.Segments)
	}
	if final.Text != "发票号码042。\n总金额¥128。\n付款状态为已支付。" {
		t.Fatalf("text = %q", final.Text)
	}
	wantStats := Stats{Engines: 3, Segments: 3, ConsensusSegments: 2, EscalatedSegments: 1, Escalator: "fake-arbiter"}
	if final.Stats != wantStats {
		t.Fatalf("stats = %+v, want %+v", final.Stats, wantStats)
	}
}

func TestFuseBatchesAllDisputesInOneEscalation(t *testing.T) {
	esc := &fakeEsc{fn: func(disputes []Dispute) ([]Resolution, error) {
		resolutions := make([]Resolution, 0, len(disputes))
		for _, dispute := range disputes {
			resolutions = append(resolutions, Resolution{
				Segment: dispute.Segment,
				Text:    "裁定:" + dispute.Candidates[0].Text,
			})
		}
		return resolutions, nil
	}}
	fuser := New()
	fuser.Escalator = esc
	final := fuser.Fuse(context.Background(), []byte("image"), []agent.Result{
		result("a", "共同内容。发票编号042。应付金额128元。"),
		result("b", "共同内容。发票编号O42。应付金额129元。"),
	})

	if len(esc.calls) != 1 {
		t.Fatalf("arbiter calls = %d, want exactly one batch", len(esc.calls))
	}
	if got := esc.calls[0]; len(got) != 2 || got[0].Segment != 1 || got[1].Segment != 2 {
		t.Fatalf("batched disputes = %+v, want segments 1 and 2", got)
	}
	if final.Stats.EscalatedSegments != 2 || final.Stats.FallbackSegments != 0 {
		t.Fatalf("stats = %+v, want both disputes escalated", final.Stats)
	}
	for _, index := range []int{1, 2} {
		segment := final.Segments[index]
		if segment.Source != SourceEscalated || !strings.HasPrefix(segment.Text, "裁定:") {
			t.Fatalf("segment %d = %+v, want escalated resolution", index, segment)
		}
	}
}

func TestFuseCanDeferArbitrationForUser(t *testing.T) {
	esc := &fakeEsc{}
	fuser := New()
	fuser.Escalator = esc
	fuser.DeferEscalation = true
	final := fuser.Fuse(context.Background(), nil, []agent.Result{
		result("a", "应付金额¥128。"),
		result("b", "应付金额128。"),
	})
	if len(esc.calls) != 0 {
		t.Fatal("deferred fusion must not call arbiter")
	}
	if len(final.Segments) != 1 {
		t.Fatalf("segments = %+v", final.Segments)
	}
	segment := final.Segments[0]
	if segment.Source != SourceFallback || !segment.Disputed || len(segment.Candidates) != 2 {
		t.Fatalf("deferred segment = %+v", segment)
	}
	if final.Stats.FallbackSegments != 1 || final.Stats.EscalatedSegments != 0 {
		t.Fatalf("stats = %+v", final.Stats)
	}
}

func TestSymbolsAlignButRemainDisputed(t *testing.T) {
	esc := &fakeEsc{fn: func(disputes []Dispute) ([]Resolution, error) {
		return []Resolution{{Segment: 0, Text: "温度-5℃。"}}, nil
	}}
	fuser := New()
	fuser.Escalator = esc
	final := fuser.Fuse(context.Background(), nil, []agent.Result{
		result("a", "温度-5℃。"), result("b", "温度5℃。"),
	})
	if len(esc.calls) != 1 || len(esc.calls[0]) != 1 {
		t.Fatalf("symbol difference was hidden: calls=%#v", esc.calls)
	}
	if final.Segments[0].Text != "温度-5℃。" {
		t.Fatalf("text = %q", final.Segments[0].Text)
	}
}

func TestEscalationFailureFallsBackWithCandidates(t *testing.T) {
	esc := &fakeEsc{fn: func([]Dispute) ([]Resolution, error) { return nil, errors.New("model failed") }}
	fuser := New()
	fuser.Escalator = esc
	final := fuser.Fuse(context.Background(), nil, []agent.Result{
		result("a", "候选甲。"), result("b", "候选乙。"),
	})
	if final.Stats.EscalationErr != "model failed" || final.Stats.FallbackSegments != 2 {
		t.Fatalf("stats = %+v", final.Stats)
	}
	for _, segment := range final.Segments {
		if segment.Source != SourceFallback || !segment.Disputed || len(segment.Candidates) != 1 {
			t.Fatalf("fallback segment = %+v", segment)
		}
	}
}

func TestEscalatorCanDropSegment(t *testing.T) {
	esc := &fakeEsc{fn: func([]Dispute) ([]Resolution, error) {
		return []Resolution{{Segment: 0, Text: ""}}, nil
	}}
	fuser := New()
	fuser.Escalator = esc
	final := fuser.Fuse(context.Background(), nil, []agent.Result{
		result("a", "模型寒暄内容。"), result("b", "正常正文。"),
	})
	if final.Stats.DroppedSegments != 1 || final.Stats.FallbackSegments != 1 {
		t.Fatalf("stats = %+v", final.Stats)
	}
}

func TestResolutionConfidence(t *testing.T) {
	candidates := []Candidate{{Agent: "a", Text: "总额 ¥128。"}, {Agent: "b", Text: "总额128。"}}
	if got := ResolutionConfidence(candidates, "总额¥128。"); got != escalatedCorroborated {
		t.Fatalf("corroborated = %v", got)
	}
	if got := ResolutionConfidence(candidates, "总额¥129。"); got != escalatedSolo {
		t.Fatalf("solo = %v", got)
	}
}

func TestFuseNoValidResults(t *testing.T) {
	final := New().Fuse(context.Background(), nil, []agent.Result{
		{Agent: "b", Err: "x"}, {Agent: "a", Err: "y"},
	})
	if final.Text != "" || final.Segments == nil || len(final.Segments) != 0 || final.Stats.FailedEngines != 2 {
		t.Fatalf("final = %+v", final)
	}
}
