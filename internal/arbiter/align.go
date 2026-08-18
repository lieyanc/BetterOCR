package arbiter

import "github.com/lieyanc/BetterOCR/internal/agent"

// cand 是某个行槽上来自单个引擎的候选。每个引擎在一个行槽上至多一条。
type cand struct {
	agent string
	text  string
}

// row 是对齐后的一个"行槽",汇集各引擎对同一物理行的候选。
type row struct {
	cands []cand
}

// rep 返回该行槽的代表候选:medoid——与组内其他候选相似度之和最高的那条。
//
// medoid 只用候选彼此的关系推导"谁最不像离群值",不依赖任何引擎自报指标。
// 三个引擎里两个读作 "Hello" 一个读作 "He110" 时,medoid 必是前者;
// 换成自报置信度择优,则完全取决于哪个模型更爱写 0.98。
// 只有两个候选时二者相似度相同,退化为字典序——那时本就没有可用信号。
//
// 平局按引擎名字典序,保证结果与 cands 的排列顺序无关、可复现。
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

// alignAll 把各引擎的行序列渐进对齐成行槽序列。
// results 需已按确定性顺序排列;逐个结果用全局序列对齐
// (Needleman-Wunsch)并入,行相似度超过 threshold 才归入同一行槽。
func alignAll(results []agent.Result, threshold float64) []*row {
	var rows []*row
	for _, res := range results {
		rows = mergeResult(rows, res, threshold)
	}
	return rows
}

const (
	moveDiag = int8(iota) // 行槽与新行配对
	moveUp                // 行槽在该引擎中无对应行
	moveLeft              // 新行插入为新行槽
)

// mergeResult 用动态规划把一个引擎的行序列对齐进现有行槽序列。
// 配对得分 = 相似度 - threshold,gap 得分 = 0:只有相似度严格超过
// 阈值的行对才值得配对,边界情况宁可分成两行留给仲裁。
func mergeResult(rows []*row, res agent.Result, threshold float64) []*row {
	m, n := len(rows), len(res.Lines)
	// 行槽锚点在 DP 之前一次性算好:rep 是候选数的平方级开销,
	// 放进 DP 内层会被重复求值 m×n 次。
	anchors := make([]string, m)
	for i, r := range rows {
		anchors[i] = r.rep().text
	}
	dp := make([][]float64, m+1)
	move := make([][]int8, m+1)
	for i := range dp {
		dp[i] = make([]float64, n+1)
		move[i] = make([]int8, n+1)
	}
	for j := 1; j <= n; j++ {
		move[0][j] = moveLeft
	}
	for i := 1; i <= m; i++ {
		move[i][0] = moveUp
	}
	for i := 1; i <= m; i++ {
		for j := 1; j <= n; j++ {
			diag := dp[i-1][j-1] + Similarity(anchors[i-1], res.Lines[j-1]) - threshold
			up := dp[i-1][j]
			left := dp[i][j-1]
			// 固定优先级保证确定性:严格更优才配对。
			switch {
			case diag > up && diag > left:
				dp[i][j], move[i][j] = diag, moveDiag
			case up >= left:
				dp[i][j], move[i][j] = up, moveUp
			default:
				dp[i][j], move[i][j] = left, moveLeft
			}
		}
	}

	// 回溯得到操作序列(倒序),再正序重建行槽列表,
	// 天然保持两个序列各自的阅读顺序。
	var ops []int8
	for i, j := m, n; i > 0 || j > 0; {
		mv := move[i][j]
		ops = append(ops, mv)
		switch mv {
		case moveDiag:
			i--
			j--
		case moveUp:
			i--
		case moveLeft:
			j--
		}
	}

	out := make([]*row, 0, m+n)
	ri, li := 0, 0
	for k := len(ops) - 1; k >= 0; k-- {
		switch ops[k] {
		case moveDiag:
			rows[ri].cands = append(rows[ri].cands, cand{agent: res.Agent, text: res.Lines[li]})
			out = append(out, rows[ri])
			ri++
			li++
		case moveUp:
			out = append(out, rows[ri])
			ri++
		case moveLeft:
			out = append(out, &row{cands: []cand{{agent: res.Agent, text: res.Lines[li]}}})
			li++
		}
	}
	return out
}
