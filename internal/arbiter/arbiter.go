// Package arbiter 对多引擎的全文结果做中文句段对齐、共识判定与分歧仲裁。
//
// 设计遵循三条第一性原理:
//
//  1. 基础模型只负责返回完整 OCR 文本;句段结构由本地确定性算法产生,
//     不依赖模型是否遵循图片物理行。
//  2. 强模型成本应与分歧量成正比。引擎一致的句段免费通过,
//     只有争议句段打包交给更强的仲裁器,或留给用户处理。
//  3. 判据必须可观测。句段可信度只从结构信号推导——多少个独立引擎
//     逐字给出了同一文本、多少个持异议、句段走了哪条路径——而不采信
//     模型的自报置信度:那个数聚集在少数整数上,与真实正确率无关。
package arbiter

import (
	"context"
	"math"
	"sort"
	"strings"

	"github.com/lieyanc/BetterOCR/internal/agent"
)

// Candidate 是分歧句段上来自某个引擎的候选文本。
type Candidate struct {
	Agent string `json:"agent"`
	Text  string `json:"text"`
}

// Dispute 是一处待仲裁的句段分歧。Before/After 是最近的上下文句段。
type Dispute struct {
	Segment    int         `json:"segment"`
	Before     string      `json:"before,omitempty"`
	After      string      `json:"after,omitempty"`
	Candidates []Candidate `json:"candidates"`
}

// Resolution 是仲裁器对一处分歧的裁定。
// Text 为空表示该句段并不存在于图中,应当丢弃。其余字段由独立仲裁
// 接口补齐,便于前端直接更新结果。
type Resolution struct {
	Segment    int      `json:"segment"`
	Text       string   `json:"text"`
	Confidence float64  `json:"confidence,omitempty"`
	From       []string `json:"from,omitempty"`
}

// Escalator 是分歧仲裁器,通常由更强的 VLM 实现。
// 一次 Resolve 调用裁定一张图上的全部分歧,调用成本与图片数而非分歧数成正比。
type Escalator interface {
	Name() string
	Resolve(ctx context.Context, image []byte, disputes []Dispute) ([]Resolution, error)
}

// 句段来源标记。
const (
	SourceConsensus = "consensus" // 多数引擎一致,直接通过
	SourceEscalated = "escalated" // 分歧句段,由仲裁器看图裁定
	SourceFallback  = "fallback"  // 待人工处理或仲裁失败,本地确定性择优
)

// FinalSegment 是融合输出中的一个动态句段。
type FinalSegment struct {
	Text string `json:"text"`
	// Confidence 是该句段正确的估计概率。
	Confidence float64 `json:"confidence"`
	// Source 标记该句段如何产生:consensus / escalated / fallback。
	Source string `json:"source"`
	// From 是该句段的依据:共识引擎列表、仲裁器名或兜底引擎名。
	From []string `json:"from"`
	// Disputed 表示原始引擎存在分歧,即使该句段已经自动仲裁。
	Disputed bool `json:"disputed,omitempty"`
	// Candidates 供用户直接合并、编辑或重新发起仲裁。
	Candidates []Candidate `json:"candidates,omitempty"`
}

// Stats 汇总一次融合的过程数据,用于观测成本与分歧率。
type Stats struct {
	Engines           int    `json:"engines"`
	FailedEngines     int    `json:"failed_engines"`
	Segments          int    `json:"segments"`
	ConsensusSegments int    `json:"consensus_segments"`
	EscalatedSegments int    `json:"escalated_segments"`
	FallbackSegments  int    `json:"fallback_segments"`
	DroppedSegments   int    `json:"dropped_segments,omitempty"`
	Escalator         string `json:"escalator,omitempty"`
	EscalationErr     string `json:"escalation_err,omitempty"`
}

// Final 是融合后的最终结果。
type Final struct {
	// Text 是各句段按序拼接的完整文本。
	Text string `json:"text"`
	// Confidence 是各句段置信度的均值。
	Confidence float64        `json:"confidence"`
	Segments   []FinalSegment `json:"segments"`
	Stats      Stats          `json:"stats"`
	// Candidates 是原始引擎结果(含失败者),按引擎名排序。
	Candidates []agent.Result `json:"candidates"`
}

// Arbiter 融合多引擎的完整文本。
type Arbiter struct {
	// AlignThreshold 正文骨架相似度需严格超过该值才归入同一句段槽。
	AlignThreshold float64
	// Escalator 裁定分歧句段。
	Escalator Escalator
	// DeferEscalation 保留争议供用户合并或手动发起仲裁。
	DeferEscalation bool
}

// New 创建默认 Arbiter。
func New() *Arbiter {
	return &Arbiter{AlignThreshold: 0.35}
}

// Fuse 融合多引擎结果:全文切句 → 句段对齐 → 共识判定 → 可选仲裁 → 汇总。
func (a *Arbiter) Fuse(ctx context.Context, image []byte, results []agent.Result) Final {
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
		if strings.TrimSpace(r.Text) != "" {
			valid = append(valid, r)
		}
	}
	if a.Escalator != nil {
		stats.Escalator = a.Escalator.Name()
	}
	if len(valid) == 0 {
		return Final{Segments: []FinalSegment{}, Stats: stats, Candidates: sorted}
	}

	rows := alignAll(valid, a.AlignThreshold)
	stats.Segments = len(rows)

	segments := make([]*FinalSegment, len(rows))
	var disputeIndexes []int
	for i, r := range rows {
		if segment, ok := consensusOf(r, len(valid)); ok {
			segments[i] = segment
			stats.ConsensusSegments++
		} else {
			disputeIndexes = append(disputeIndexes, i)
		}
	}

	if len(disputeIndexes) > 0 {
		disputes := buildDisputes(rows, segments, disputeIndexes)
		disputeByIndex := make(map[int]Dispute, len(disputes))
		for _, dispute := range disputes {
			disputeByIndex[dispute.Segment] = dispute
		}
		resolved := map[int]Resolution{}
		if a.Escalator != nil && !a.DeferEscalation {
			if rs, err := a.Escalator.Resolve(ctx, image, disputes); err != nil {
				stats.EscalationErr = err.Error()
			} else {
				inDispute := make(map[int]bool, len(disputeIndexes))
				for _, i := range disputeIndexes {
					inDispute[i] = true
				}
				for _, res := range rs {
					if inDispute[res.Segment] {
						resolved[res.Segment] = res
					}
				}
			}
		}
		for _, i := range disputeIndexes {
			candidates := disputeByIndex[i].Candidates
			if res, ok := resolved[i]; ok {
				stats.EscalatedSegments++
				if strings.TrimSpace(res.Text) == "" {
					stats.DroppedSegments++
					continue
				}
				segments[i] = &FinalSegment{
					Text:       res.Text,
					Confidence: escalatedConfidence(rows[i], res.Text),
					Source:     SourceEscalated,
					From:       []string{a.Escalator.Name()},
					Disputed:   true,
					Candidates: candidates,
				}
			} else {
				c := rows[i].rep()
				segments[i] = &FinalSegment{
					Text:       c.text,
					Confidence: round4(fallbackConfidence(rows[i], c)),
					Source:     SourceFallback,
					From:       []string{c.agent},
					Disputed:   true,
					Candidates: candidates,
				}
				stats.FallbackSegments++
			}
		}
	}

	finalSegments := make([]FinalSegment, 0, len(segments))
	texts := make([]string, 0, len(segments))
	confSum := 0.0
	for _, segment := range segments {
		if segment == nil {
			continue
		}
		finalSegments = append(finalSegments, *segment)
		texts = append(texts, segment.Text)
		confSum += segment.Confidence
	}
	f := Final{
		Text:       strings.Join(texts, "\n"),
		Segments:   finalSegments,
		Stats:      stats,
		Candidates: sorted,
	}
	if len(finalSegments) > 0 {
		f.Confidence = round4(confSum / float64(len(finalSegments)))
	}
	return f
}

// ── 置信度模型 ────────────────────────────────────────────────────────
//
// 置信度不来自模型自报。VLM 的自报值聚集在 0.9 / 0.95 / 0.98 这几个整数上,
// 与该句段是否真的正确几乎无关——尤其在 OCR 最容易错的地方(0/O、1/l、己/已),
// 模型恰恰是自信地读错。把它当决策依据,等于给随机数配上刻度。
//
// 这里的置信度全部由结构信号推导:多少个独立引擎逐字给出了同一文本、
// 多少个引擎持异议、这一句段最终走了哪条路径。这些量可观测、可复现,
// 且不额外消耗任何 token。
const (
	// engineErrRate 是单个引擎的句段错误率先验:一个引擎独自给出的句段,
	// 大致有这么大概率是错的。
	engineErrRate = 0.20
	// engineCorr 是引擎间的错误相关性折扣。同代 VLM 会在同一个模糊字形上
	// 犯同样的错——一致不等于独立,所以第 k 个一致引擎只计 engineCorr 份证据。
	// 取 1 就退化成"完全独立"的乐观假设,那正是旧的 1-Π(1-ci) 会饱和到
	// 0.999 的原因。
	engineCorr = 0.6
	// dissentPenalty 是异议的折减权重:某个有效引擎没有给出这一文本
	// (给了别的文本,或压根没在这个位置产出句段),每一份都会拉低置信度。
	dissentPenalty = 0.5

	// escalatedCorroborated 是仲裁裁定与某个引擎候选逐字吻合时的置信度:
	// "看图"与"某个独立引擎"两条证据链指向同一处。
	escalatedCorroborated = 0.95
	// escalatedSolo 是仲裁裁定不同于任何候选时的置信度:它可能读出了
	// 所有引擎都读错的字,也可能是自己在幻觉,没有旁证可以分辨。
	escalatedSolo = 0.80

	// 兜底句段(无仲裁器,或仲裁失败/漏答)的置信度区间。上限刻意压在 0.7
	// 以下:本地候选是"没得选才选它",不应显示得和共识句段一样可信。
	fallbackCeil  = 0.65
	fallbackFloor = 0.30
	// fallbackLone 用于只有一个引擎看到的孤立句段:没有任何旁证,
	// 它既可能是别人漏掉的真实句段,也可能是这个引擎的幻觉。
	fallbackLone = 0.25
)

// consensusConfidence 由逐字一致的引擎数 k 与有效引擎数 n 推导共识句段置信度。
//
// 一致引擎按相关性折扣累计为 evidence 份独立证据,错误概率取 engineErrRate
// 的 evidence 次幂;再按异议比例 (n-k)/n 线性折减。典型取值:
//
//	k=2,n=2 → 0.924   k=3,n=3 → 0.971   k=5,n=5 → 0.996
//	k=2,n=3 → 0.770   k=3,n=5 → 0.777   k=4,n=5 → 0.890
//
// 这个刻度是有区分度的:两家一致、第三家不同的句段会落到 0.77,
// 在界面上就该与全体一致的句段明显区分。
func consensusConfidence(k, n int) float64 {
	evidence := 1 + float64(k-1)*engineCorr
	conf := 1 - math.Pow(engineErrRate, evidence)
	if n > k {
		conf *= 1 - dissentPenalty*float64(n-k)/float64(n)
	}
	return conf
}

// escalatedConfidence 区分仲裁裁定是否拿到了独立旁证。
func escalatedConfidence(r *row, text string) float64 {
	norm := Normalize(text)
	for _, c := range r.cands {
		if Normalize(c.text) == norm {
			return escalatedCorroborated
		}
	}
	return escalatedSolo
}

// fallbackConfidence 由候选彼此的接近程度推导兜底句段置信度:候选只差
// 一两个字符时,选错的代价有限;彼此面目全非时,这一句段基本是掷硬币。
func fallbackConfidence(r *row, rep cand) float64 {
	if len(r.cands) < 2 {
		return fallbackLone
	}
	sum := 0.0
	for _, c := range r.cands {
		if c.agent != rep.agent {
			sum += Similarity(rep.text, c.text)
		}
	}
	mean := sum / float64(len(r.cands)-1)
	return fallbackFloor + (fallbackCeil-fallbackFloor)*mean
}

// consensusOf 判定一个句段槽是否形成保留符号的逐字共识。
//
// 判据:规整后逐字相同的引擎数 k 满足 k ≥ 2 且 k 构成全体有效引擎 n 的
// 严格多数(2k > n)。严格多数在数学上唯一,map 迭代顺序不影响结果。
//
// 分母取 n 而不是该句段槽上的候选数:某个引擎在这里完全没有产出候选,
// 本身就是"该内容可能根本不存在"的证据,不该被
// 忽略。旧判据下,5 个引擎里只有 2 个看到某句段、且这 2 个一致,就会当作
// 共识直接通过;新判据把它判为分歧,交给仲裁器看图定夺。
func consensusOf(r *row, n int) (*FinalSegment, bool) {
	counts := make(map[string]int, len(r.cands))
	for _, c := range r.cands {
		counts[Normalize(c.text)]++
	}
	for norm, k := range counts {
		if k < 2 || k*2 <= n {
			continue
		}
		from := make([]string, 0, k)
		text, picked := "", false
		for _, c := range r.cands {
			if Normalize(c.text) != norm {
				continue
			}
			if !picked {
				// cands 按引擎名升序,取首个即确定性代表;
				// 同组候选规整后逐字相同,差异只在排版空白。
				text, picked = c.text, true
			}
			from = append(from, c.agent)
		}
		sort.Strings(from)
		return &FinalSegment{
			Text:       text,
			Confidence: round4(consensusConfidence(k, n)),
			Source:     SourceConsensus,
			From:       from,
		}, true
	}
	return nil, false
}

// buildDisputes 为待仲裁句段构造 Dispute,附上最近的上下文共识句段。
func buildDisputes(rows []*row, segments []*FinalSegment, disputeIndexes []int) []Dispute {
	disputes := make([]Dispute, 0, len(disputeIndexes))
	for _, i := range disputeIndexes {
		d := Dispute{Segment: i}
		for j := i - 1; j >= 0; j-- {
			if segments[j] != nil {
				d.Before = segments[j].Text
				break
			}
		}
		for j := i + 1; j < len(segments); j++ {
			if segments[j] != nil {
				d.After = segments[j].Text
				break
			}
		}
		for _, c := range rows[i].cands {
			d.Candidates = append(d.Candidates, Candidate{Agent: c.agent, Text: c.text})
		}
		disputes = append(disputes, d)
	}
	return disputes
}

// ResolutionConfidence 为独立的手动仲裁接口计算与自动仲裁一致的置信度。
func ResolutionConfidence(candidates []Candidate, text string) float64 {
	norm := Normalize(text)
	for _, candidate := range candidates {
		if Normalize(candidate.Text) == norm {
			return escalatedCorroborated
		}
	}
	return escalatedSolo
}

func round4(x float64) float64 {
	return math.Round(x*1e4) / 1e4
}
