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
	moveSpan // 已有多个句段 ↔ 新结果多个句段
)

const (
	// maxAlignSpan 限制一次转移合并的边界数。更大的无锚点区域会在融合层
	// 继续合成争议块,因此这里无需用无界搜索拖慢正常页面。
	maxAlignSpan = 8
	// spanBoundaryPenalty 只用于打破“保留边界”和“合并边界”的竞争。
	// 多个可靠的 1:1 匹配天然会获得更高总分,这里保持很小即可。
	spanBoundaryPenalty = 0.02
)

type step struct {
	move  int8
	prevI int
	prevJ int
}

// mergeSegments 使用有界多对多动态规划对齐句段序列。不同 OCR 模型常把
// 同一段分别切成 1、3、5 个句段;只支持 1:2 / 2:1 会把这些替代读法排成
// 多份正文。span 转移比较拼接后的内容,同时让可靠的细粒度匹配优先。
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
	anchorCores := alignmentSpanFingerprints(anchors)
	textCores := alignmentSpanFingerprints(texts)
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
			if similarity, ok := anchorCores[i][1].similarityAbove(textCores[j][1], threshold); ok {
				if score := dp[i-1][j-1] + similarity - threshold; score > best {
					best, choice = score, step{move: moveDiag, prevI: i - 1, prevJ: j - 1}
				}
			}
			maxRows := min(i, maxAlignSpan)
			maxTexts := min(j, maxAlignSpan)
			for rowSpan := 1; rowSpan <= maxRows; rowSpan++ {
				for textSpan := 1; textSpan <= maxTexts; textSpan++ {
					if rowSpan == 1 && textSpan == 1 {
						continue
					}
					penalty := spanBoundaryPenalty * float64(rowSpan+textSpan-2)
					similarity, ok := anchorCores[i][rowSpan].similarityAbove(
						textCores[j][textSpan], threshold+penalty,
					)
					if !ok {
						continue
					}
					score := dp[i-rowSpan][j-textSpan] + similarity - threshold - penalty
					if score > best {
						best = score
						choice = step{move: moveSpan, prevI: i - rowSpan, prevJ: j - textSpan}
					}
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
		case moveSpan:
			r := combineRows(rows[op.i0:op.i1])
			r.cands = append(r.cands, cand{
				agent: agentName,
				text:  strings.Join(texts[op.j0:op.j1], ""),
			})
			out = append(out, r)
		}
	}
	return out
}

// alignmentSpanFingerprints 缓存每个结束位置向前 1..maxAlignSpan 个句段的
// 正文骨架及 bigram 集合,让 DP 内的相似度比较不再重复分配 map。
func alignmentSpanFingerprints(texts []string) [][]coreFingerprint {
	out := make([][]coreFingerprint, len(texts)+1)
	for end := 1; end <= len(texts); end++ {
		limit := min(end, maxAlignSpan)
		out[end] = make([]coreFingerprint, limit+1)
		combined := ""
		for span := 1; span <= limit; span++ {
			combined = texts[end-span] + combined
			out[end][span] = newCoreFingerprint(CoreNormalize(combined))
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

func combineRows(rows []*row) *row {
	byAgent := make(map[string][]string)
	for _, row := range rows {
		for _, c := range row.cands {
			byAgent[c.agent] = append(byAgent[c.agent], c.text)
		}
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
