// Package arbiter 对多引擎的行级结果做对齐、共识判定与分歧仲裁。
//
// 设计遵循两条第一性原理:
//
//  1. 融合必须发生在行级。整篇选择的准确率上限是最好的单引擎;
//     行级组合才可能超过任何单个引擎(各引擎错在不同的行)。
//  2. 强模型成本应与分歧量成正比。引擎一致的行免费通过,
//     只有分歧行(以及无旁证的孤行)打包一次交给更强的仲裁器。
package arbiter

import (
	"context"
	"math"
	"sort"
	"strings"

	"github.com/lieyanc/BetterOCR/internal/agent"
)

// Candidate 是分歧行上来自某个引擎的候选文本。
type Candidate struct {
	Agent      string  `json:"agent"`
	Text       string  `json:"text"`
	Confidence float64 `json:"confidence"`
}

// Dispute 是一处待仲裁的分歧。Before/After 是最近的上下文共识行,
// 帮助仲裁器在原图中定位该行。
type Dispute struct {
	Row        int         `json:"row"`
	Before     string      `json:"before,omitempty"`
	After      string      `json:"after,omitempty"`
	Candidates []Candidate `json:"candidates"`
}

// Resolution 是仲裁器对一处分歧的裁定。
// Text 为空表示该行并不存在于图中,应当丢弃。
type Resolution struct {
	Row        int
	Text       string
	Confidence float64
}

// Escalator 是分歧仲裁器,通常由更强的 VLM 实现。
// 一次 Resolve 调用裁定一张图上的全部分歧,调用成本与图片数而非分歧数成正比。
type Escalator interface {
	Name() string
	Resolve(ctx context.Context, image []byte, disputes []Dispute) ([]Resolution, error)
}

// 行来源标记。
const (
	SourceConsensus = "consensus" // 多数引擎一致,直接通过
	SourceEscalated = "escalated" // 分歧行,由仲裁器看图裁定
	SourceFallback  = "fallback"  // 无仲裁器或仲裁失败,本地确定性择优
)

// FinalLine 是融合输出中的一行。
type FinalLine struct {
	Text       string  `json:"text"`
	Confidence float64 `json:"confidence"`
	// Source 标记该行如何产生:consensus / escalated / fallback。
	Source string `json:"source"`
	// From 是该行的依据:共识引擎列表、仲裁器名或兜底引擎名。
	From []string `json:"from"`
}

// Stats 汇总一次融合的过程数据,用于观测成本与分歧率。
type Stats struct {
	Engines       int    `json:"engines"`
	FailedEngines int    `json:"failed_engines"`
	Rows          int    `json:"rows"`
	ConsensusRows int    `json:"consensus_rows"`
	EscalatedRows int    `json:"escalated_rows"`
	FallbackRows  int    `json:"fallback_rows"`
	DroppedRows   int    `json:"dropped_rows,omitempty"` // 仲裁认定图中不存在的行
	Escalator     string `json:"escalator,omitempty"`
	EscalationErr string `json:"escalation_err,omitempty"`
}

// Final 是融合后的最终结果。
type Final struct {
	// Text 是各行按序拼接的完整文本。
	Text string `json:"text"`
	// Confidence 是各行置信度的均值。
	Confidence float64     `json:"confidence"`
	Lines      []FinalLine `json:"lines"`
	Stats      Stats       `json:"stats"`
	// Candidates 是原始引擎结果(含失败者),按引擎名排序。
	Candidates []agent.Result `json:"candidates"`
}

// Arbiter 融合多引擎行级结果。
type Arbiter struct {
	// AlignThreshold 行相似度需严格超过该值才归入同一行槽。
	AlignThreshold float64
	// Escalator 裁定分歧行;为 nil 时分歧行退化为本地择优(仅保底,不推荐)。
	Escalator Escalator
}

// New 创建默认 Arbiter。
func New() *Arbiter {
	return &Arbiter{AlignThreshold: 0.35}
}

// Fuse 融合多引擎结果:对齐 → 共识判定 → 分歧仲裁 → 汇总。
// image 是原始图片,供仲裁器复核;ctx 约束仲裁调用。
func (a *Arbiter) Fuse(ctx context.Context, image []byte, results []agent.Result) Final {
	// 不信任调用方顺序,统一按引擎名排序,保证全流程确定性。
	sorted := make([]agent.Result, len(results))
	copy(sorted, results)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Agent < sorted[j].Agent })

	valid := make([]agent.Result, 0, len(sorted))
	stats := Stats{Engines: len(sorted)}
	for _, r := range sorted {
		if r.Err != "" {
			stats.FailedEngines++
			continue
		}
		if len(r.Lines) > 0 {
			valid = append(valid, r)
		}
	}
	if a.Escalator != nil {
		stats.Escalator = a.Escalator.Name()
	}
	if len(valid) == 0 {
		return Final{Lines: []FinalLine{}, Stats: stats, Candidates: sorted}
	}

	rows := alignAll(valid, a.AlignThreshold)
	stats.Rows = len(rows)

	// 第一遍:共识判定。lines[i] 为 nil 表示该行槽待仲裁(或最终被丢弃)。
	lines := make([]*FinalLine, len(rows))
	var disputeRows []int
	for i, r := range rows {
		if fl, ok := consensusOf(r); ok {
			lines[i] = fl
			stats.ConsensusRows++
		} else {
			disputeRows = append(disputeRows, i)
		}
	}

	// 第二遍:分歧行打包仲裁;仲裁不可用或漏答时本地兜底。
	// 仲裁失败绝不拖垮整体——与"单引擎失败不影响其他引擎"同一条原则。
	if len(disputeRows) > 0 {
		disputes := buildDisputes(rows, lines, disputeRows)
		resolved := map[int]Resolution{}
		if a.Escalator != nil {
			if rs, err := a.Escalator.Resolve(ctx, image, disputes); err != nil {
				stats.EscalationErr = err.Error()
			} else {
				inDispute := make(map[int]bool, len(disputeRows))
				for _, i := range disputeRows {
					inDispute[i] = true
				}
				for _, res := range rs {
					if inDispute[res.Row] {
						resolved[res.Row] = res
					}
				}
			}
		}
		for _, i := range disputeRows {
			if res, ok := resolved[i]; ok {
				stats.EscalatedRows++
				if strings.TrimSpace(res.Text) == "" {
					stats.DroppedRows++ // 仲裁认定该行不存在,lines[i] 保持 nil
					continue
				}
				lines[i] = &FinalLine{
					Text:       res.Text,
					Confidence: round4(clamp01(res.Confidence)),
					Source:     SourceEscalated,
					From:       []string{a.Escalator.Name()},
				}
			} else {
				c := rows[i].rep()
				lines[i] = &FinalLine{
					Text:       c.line.Text,
					Confidence: round4(clamp01(c.line.Confidence)),
					Source:     SourceFallback,
					From:       []string{c.agent},
				}
				stats.FallbackRows++
			}
		}
	}

	finalLines := make([]FinalLine, 0, len(lines))
	texts := make([]string, 0, len(lines))
	confSum := 0.0
	for _, l := range lines {
		if l == nil {
			continue
		}
		finalLines = append(finalLines, *l)
		texts = append(texts, l.Text)
		confSum += l.Confidence
	}
	f := Final{
		Text:       strings.Join(texts, "\n"),
		Lines:      finalLines,
		Stats:      stats,
		Candidates: sorted,
	}
	if len(finalLines) > 0 {
		f.Confidence = round4(confSum / float64(len(finalLines)))
	}
	return f
}

// consensusOf 判定一个行槽是否形成共识:至少两个引擎给出归一化后
// 完全相同的文本,且构成严格多数(> 半数,保证唯一)。
// 共识置信度按独立证据近似合成:1 - Π(1-ci)。
// 一致性本身就是证据——三个引擎各 0.9 且一致,比单引擎 0.92 更可信。
func consensusOf(r *row) (*FinalLine, bool) {
	counts := make(map[string]int, len(r.cands))
	for _, c := range r.cands {
		counts[Normalize(c.line.Text)]++
	}
	for norm, cnt := range counts {
		if cnt < 2 || cnt*2 <= len(r.cands) {
			continue
		}
		// 严格多数在数学上唯一,map 迭代顺序不影响结果。
		members := make([]cand, 0, cnt)
		for _, c := range r.cands {
			if Normalize(c.line.Text) == norm {
				members = append(members, c)
			}
		}
		repr := members[0]
		miss := 1.0
		from := make([]string, 0, len(members))
		for _, m := range members {
			miss *= 1 - clamp01(m.line.Confidence)
			from = append(from, m.agent)
			if m.line.Confidence > repr.line.Confidence ||
				(m.line.Confidence == repr.line.Confidence && m.agent < repr.agent) {
				repr = m
			}
		}
		sort.Strings(from)
		return &FinalLine{
			Text:       repr.line.Text,
			Confidence: round4(1 - miss),
			Source:     SourceConsensus,
			From:       from,
		}, true
	}
	return nil, false
}

// buildDisputes 为待仲裁行槽构造 Dispute,附上最近的上下文共识行。
func buildDisputes(rows []*row, lines []*FinalLine, disputeRows []int) []Dispute {
	disputes := make([]Dispute, 0, len(disputeRows))
	for _, i := range disputeRows {
		d := Dispute{Row: i}
		for j := i - 1; j >= 0; j-- {
			if lines[j] != nil {
				d.Before = lines[j].Text
				break
			}
		}
		for j := i + 1; j < len(lines); j++ {
			if lines[j] != nil {
				d.After = lines[j].Text
				break
			}
		}
		for _, c := range rows[i].cands {
			d.Candidates = append(d.Candidates, Candidate{
				Agent:      c.agent,
				Text:       c.line.Text,
				Confidence: c.line.Confidence,
			})
		}
		disputes = append(disputes, d)
	}
	return disputes
}

func clamp01(x float64) float64 {
	return math.Min(1, math.Max(0, x))
}

func round4(x float64) float64 {
	return math.Round(x*1e4) / 1e4
}
