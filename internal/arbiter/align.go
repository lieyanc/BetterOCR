package arbiter

import (
	"sort"
	"strings"

	"github.com/lieyanc/BetterOCR/internal/agent"
)

// cand 是某个句段槽上来自单个引擎的候选。每个引擎在一个槽中至多一条。
type cand struct {
	agent string
	text  string
}

// row 保留这个内部名称以限制改动范围;语义上它已经是动态句段槽,不是物理行。
type row struct {
	cands []cand
}

// rep 返回与组内其他候选最接近的 medoid,平局按引擎名保证确定性。
func (r *row) rep() cand {
	best, bestScore := r.cands[0], -1.0
	for _, c := range r.cands {
		score := 0.0
		for _, other := range r.cands {
			if other.agent != c.agent {
				score += Similarity(c.text, other.text)
			}
		}
		if score > bestScore || (score == bestScore && c.agent < best.agent) {
			best, bestScore = c, score
		}
	}
	return best
}

// alignAll 先独立切分每个模型的全文,再渐进对齐句段序列。
func alignAll(results []agent.Result, threshold float64) []*row {
	var rows []*row
	for _, res := range results {
		rows = mergeSegments(rows, res.Agent, SplitSegments(res.Text), threshold)
	}
	return rows
}

const (
	moveDiag = int8(iota)
	moveUp
	moveLeft
	moveRowsPair // 已有两个句段 ↔ 新结果一个句段
	moveTextPair // 已有一个句段 ↔ 新结果两个句段
)

type step struct {
	move  int8
	prevI int
	prevJ int
}

// mergeSegments 使用带 1:2 / 2:1 转移的序列对齐。额外两种转移专门吸收
// 模型漏掉或多写句末标点造成的边界差异。
func mergeSegments(rows []*row, agentName string, texts []string, threshold float64) []*row {
	if len(rows) == 0 {
		out := make([]*row, 0, len(texts))
		for _, text := range texts {
			out = append(out, &row{cands: []cand{{agent: agentName, text: text}}})
		}
		return out
	}
	m, n := len(rows), len(texts)
	anchors := make([]string, m)
	for i, r := range rows {
		anchors[i] = r.rep().text
	}
	dp := make([][]float64, m+1)
	trace := make([][]step, m+1)
	for i := range dp {
		dp[i] = make([]float64, n+1)
		trace[i] = make([]step, n+1)
	}
	for j := 1; j <= n; j++ {
		trace[0][j] = step{move: moveLeft, prevJ: j - 1}
	}
	for i := 1; i <= m; i++ {
		trace[i][0] = step{move: moveUp, prevI: i - 1}
	}
	for i := 1; i <= m; i++ {
		for j := 1; j <= n; j++ {
			// 固定平局优先级为 gap > 配对,因此只有相似度严格超过阈值
			// 的候选才会进入同一个句段槽。
			best := dp[i-1][j]
			choice := step{move: moveUp, prevI: i - 1, prevJ: j}
			if score := dp[i][j-1]; score > best {
				best, choice = score, step{move: moveLeft, prevI: i, prevJ: j - 1}
			}
			if score := dp[i-1][j-1] + Similarity(anchors[i-1], texts[j-1]) - threshold; score > best {
				best, choice = score, step{move: moveDiag, prevI: i - 1, prevJ: j - 1}
			}
			if i >= 2 {
				combined := anchors[i-2] + anchors[i-1]
				if score := dp[i-2][j-1] + Similarity(combined, texts[j-1]) - threshold - 0.05; score > best {
					best, choice = score, step{move: moveRowsPair, prevI: i - 2, prevJ: j - 1}
				}
			}
			if j >= 2 {
				combined := texts[j-2] + texts[j-1]
				if score := dp[i-1][j-2] + Similarity(anchors[i-1], combined) - threshold - 0.05; score > best {
					best, choice = score, step{move: moveTextPair, prevI: i - 1, prevJ: j - 2}
				}
			}
			dp[i][j], trace[i][j] = best, choice
		}
	}

	var reversed []alignedOp
	for i, j := m, n; i > 0 || j > 0; {
		s := trace[i][j]
		reversed = append(reversed, alignedOp{move: s.move, i0: s.prevI, i1: i, j0: s.prevJ, j1: j})
		i, j = s.prevI, s.prevJ
	}
	out := make([]*row, 0, m+n)
	for k := len(reversed) - 1; k >= 0; k-- {
		op := reversed[k]
		switch op.move {
		case moveDiag:
			r := cloneRow(rows[op.i0])
			r.cands = append(r.cands, cand{agent: agentName, text: texts[op.j0]})
			out = append(out, r)
		case moveUp:
			out = append(out, cloneRow(rows[op.i0]))
		case moveLeft:
			out = append(out, &row{cands: []cand{{agent: agentName, text: texts[op.j0]}}})
		case moveRowsPair:
			r := combineRows(rows[op.i0], rows[op.i0+1])
			r.cands = append(r.cands, cand{agent: agentName, text: texts[op.j0]})
			out = append(out, r)
		case moveTextPair:
			r := cloneRow(rows[op.i0])
			r.cands = append(r.cands, cand{agent: agentName, text: texts[op.j0] + texts[op.j0+1]})
			out = append(out, r)
		}
	}
	return out
}

type alignedOp struct {
	move   int8
	i0, i1 int
	j0, j1 int
}

func cloneRow(src *row) *row {
	return &row{cands: append([]cand(nil), src.cands...)}
}

func combineRows(a, b *row) *row {
	byAgent := make(map[string][]string, len(a.cands)+len(b.cands))
	all := append(append([]cand(nil), a.cands...), b.cands...)
	for _, c := range all {
		byAgent[c.agent] = append(byAgent[c.agent], c.text)
	}
	agents := make([]string, 0, len(byAgent))
	for name := range byAgent {
		agents = append(agents, name)
	}
	sort.Strings(agents)
	out := &row{cands: make([]cand, 0, len(agents))}
	for _, name := range agents {
		out.cands = append(out.cands, cand{agent: name, text: strings.Join(byAgent[name], "")})
	}
	return out
}
