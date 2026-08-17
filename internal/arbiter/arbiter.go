// Package arbiter 对多个 Agent 的识别结果进行融合与决策。
package arbiter

import (
	"sort"
	"strings"

	"github.com/lieyanc/BetterOCR/internal/agent"
)

// Arbiter 根据置信度与文本相似度融合多 Agent 结果。
type Arbiter struct {
	// MinConfidence 低于该阈值的结果在融合前被丢弃。
	MinConfidence float64
}

// New 创建默认 Arbiter（MinConfidence = 0.3）。
func New() *Arbiter {
	return &Arbiter{MinConfidence: 0.3}
}

// Final 是融合后的最终结果。
type Final struct {
	// Text 是融合得出的最终文本。
	Text string `json:"text"`
	// Confidence 是最终结果的置信度。
	Confidence float64 `json:"confidence"`
	// Votes 是参与投票每个 Agent 的票数。
	Votes map[string]int `json:"votes"`
	// Candidates 是按得分排序的候选结果。
	Candidates []agent.Result `json:"candidates"`
}

// Fuse 融合多 Agent 结果：过滤失败与低置信度结果后，
// 以归一化相似度做加权投票，选出最佳文本。
func (a *Arbiter) Fuse(results []agent.Result) Final {
	valid := make([]agent.Result, 0, len(results))
	for _, r := range results {
		if r.Err == "" && r.Confidence >= a.MinConfidence && strings.TrimSpace(r.Text) != "" {
			valid = append(valid, r)
		}
	}
	if len(valid) == 0 {
		return Final{Votes: map[string]int{}}
	}

	// 每个候选与其他所有候选的平均相似度 * 自身置信度 = 得分
	scores := make([]float64, len(valid))
	votes := make(map[string]int)
	best := 0
	for i, ri := range valid {
		simSum := 0.0
		for j, rj := range valid {
			if i == j {
				continue
			}
			simSum += Similarity(ri.Text, rj.Text)
		}
		scores[i] = (simSum / float64(len(valid)-1)) * ri.Confidence
		if scores[i] > scores[best] {
			best = i
		}
	}
	// 与最佳文本相似度 >= 0.8 视为投它一票
	for _, rj := range valid {
		if Similarity(valid[best].Text, rj.Text) >= 0.8 {
			votes[rj.Agent] = 1
		} else {
			votes[rj.Agent] = 0
		}
	}

	sorted := make([]agent.Result, len(valid))
	copy(sorted, valid)
	sort.SliceStable(sorted, func(x, y int) bool {
		return scores[x] > scores[y]
	})

	return Final{
		Text:       valid[best].Text,
		Confidence: valid[best].Confidence,
		Votes:      votes,
		Candidates: sorted,
	}
}
