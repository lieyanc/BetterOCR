// Package arbiter 对多引擎的行级结果做对齐、共识判定与分歧仲裁。
//
// 设计遵循三条第一性原理:
//
//  1. 融合必须发生在行级。整篇选择的准确率上限是最好的单引擎;
//     行级组合才可能超过任何单个引擎(各引擎错在不同的行)。
//  2. 强模型成本应与分歧量成正比。引擎一致的行免费通过,
//     只有分歧行(以及无旁证的孤行)打包一次交给更强的仲裁器。
//  3. 判据必须可观测。行的可信度只从结构信号推导——多少个独立引擎
//     逐字给出了同一文本、多少个持异议、这行走了哪条路径——而不采信
//     模型的自报置信度:那个数聚集在少数整数上,与真实正确率无关。
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
	Agent string `json:"agent"`
	Text  string `json:"text"`
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
	Row  int
	Text string
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
	Text string `json:"text"`
	// Confidence 是该行正确的估计概率,由结构信号推导,见"置信度模型"一节。
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
	// Confidence 是各行置信度的均值,见"置信度模型"一节。
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
	// 共识判据的分母是有效引擎总数,不是该行槽上的候选数——见 consensusOf。
	lines := make([]*FinalLine, len(rows))
	var disputeRows []int
	for i, r := range rows {
		if fl, ok := consensusOf(r, len(valid)); ok {
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
					Confidence: escalatedConfidence(rows[i], res.Text),
					Source:     SourceEscalated,
					From:       []string{a.Escalator.Name()},
				}
			} else {
				c := rows[i].rep()
				lines[i] = &FinalLine{
					Text:       c.text,
					Confidence: round4(fallbackConfidence(rows[i], c)),
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

// ── 置信度模型 ────────────────────────────────────────────────────────
//
// 置信度不来自模型自报。VLM 的自报值聚集在 0.9 / 0.95 / 0.98 这几个整数上,
// 与该行是否真的正确几乎无关——尤其在 OCR 最容易错的地方(0/O、1/l、己/已),
// 模型恰恰是自信地读错。把它当决策依据,等于给随机数配上刻度。
//
// 这里的置信度全部由结构信号推导:多少个独立引擎逐字给出了同一文本、
// 多少个引擎持异议、这一行最终走了哪条路径。这些量可观测、可复现,
// 且不额外消耗任何 token。
const (
	// engineErrRate 是单个引擎的行级错误率先验:一个引擎独自给出的行,
	// 大致有这么大概率是错的。
	engineErrRate = 0.20
	// engineCorr 是引擎间的错误相关性折扣。同代 VLM 会在同一个模糊字形上
	// 犯同样的错——一致不等于独立,所以第 k 个一致引擎只计 engineCorr 份证据。
	// 取 1 就退化成"完全独立"的乐观假设,那正是旧的 1-Π(1-ci) 会饱和到
	// 0.999 的原因。
	engineCorr = 0.6
	// dissentPenalty 是异议的折减权重:某个有效引擎没有给出这一文本
	// (给了别的文本,或压根没在这个位置产出行),每一份都会拉低置信度。
	dissentPenalty = 0.5

	// escalatedCorroborated 是仲裁裁定与某个引擎候选逐字吻合时的置信度:
	// "看图"与"某个独立引擎"两条证据链指向同一处。
	escalatedCorroborated = 0.95
	// escalatedSolo 是仲裁裁定不同于任何候选时的置信度:它可能读出了
	// 所有引擎都读错的字,也可能是自己在幻觉,没有旁证可以分辨。
	escalatedSolo = 0.80

	// 兜底行(无仲裁器,或仲裁失败/漏答)的置信度区间。上限刻意压在 0.7
	// 以下:兜底是"没得选才选它",永远不该显示得和共识行一样可信。
	fallbackCeil  = 0.65
	fallbackFloor = 0.30
	// fallbackLone 用于只有一个引擎看到的孤行:没有任何旁证,
	// 它既可能是别人漏掉的真行,也可能是这个引擎的幻觉。
	fallbackLone = 0.25
)

// consensusConfidence 由逐字一致的引擎数 k 与有效引擎数 n 推导共识行置信度。
//
// 一致引擎按相关性折扣累计为 evidence 份独立证据,错误概率取 engineErrRate
// 的 evidence 次幂;再按异议比例 (n-k)/n 线性折减。典型取值:
//
//	k=2,n=2 → 0.924   k=3,n=3 → 0.971   k=5,n=5 → 0.996
//	k=2,n=3 → 0.770   k=3,n=5 → 0.777   k=4,n=5 → 0.890
//
// 这个刻度是有区分度的:两家一致、第三家不同的行会落到 0.77,
// 在界面上就该是黄色而不是和全体一致的行一样绿。
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

// fallbackConfidence 由候选彼此的接近程度推导兜底行置信度:候选只差
// 一两个字符时,选错的代价有限;彼此面目全非时,这一行基本是掷硬币。
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

// consensusOf 判定一个行槽是否形成共识,并给出该行。
//
// 判据:规整后逐字相同的引擎数 k 满足 k ≥ 2 且 k 构成全体有效引擎 n 的
// 严格多数(2k > n)。严格多数在数学上唯一,map 迭代顺序不影响结果。
//
// 分母取 n 而不是该行槽上的候选数,是可靠性上的关键一点:某个引擎在这
// 一行完全没有产出候选,本身就是"这行可能根本不存在"的证据,不该被
// 忽略。旧判据下,5 个引擎里只有 2 个看到某行、且这 2 个一致,就会当作
// 共识直接通过;新判据把它判为分歧,交给仲裁器看图定夺。
func consensusOf(r *row, n int) (*FinalLine, bool) {
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
		return &FinalLine{
			Text:       text,
			Confidence: round4(consensusConfidence(k, n)),
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
			d.Candidates = append(d.Candidates, Candidate{Agent: c.agent, Text: c.text})
		}
		disputes = append(disputes, d)
	}
	return disputes
}

func round4(x float64) float64 {
	return math.Round(x*1e4) / 1e4
}
