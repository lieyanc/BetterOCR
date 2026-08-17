package arbiter

import "github.com/lieyanc/BetterOCR/internal/agent"

// cand 是某个行槽上来自单个引擎的候选。
type cand struct {
	agent string
	line  agent.Line
}

// row 是对齐后的一个"行槽",汇集各引擎对同一物理行的候选。
type row struct {
	cands []cand
}

// rep 返回该行的代表候选:置信度最高者,平局按引擎名字典序,保证确定性。
func (r *row) rep() cand {
	best := r.cands[0]
	for _, c := range r.cands[1:] {
		if c.line.Confidence > best.line.Confidence ||
			(c.line.Confidence == best.line.Confidence && c.agent < best.agent) {
			best = c
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
			diag := dp[i-1][j-1] + Similarity(rows[i-1].rep().line.Text, res.Lines[j-1].Text) - threshold
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
			rows[ri].cands = append(rows[ri].cands, cand{agent: res.Agent, line: res.Lines[li]})
			out = append(out, rows[ri])
			ri++
			li++
		case moveUp:
			out = append(out, rows[ri])
			ri++
		case moveLeft:
			out = append(out, &row{cands: []cand{{agent: res.Agent, line: res.Lines[li]}}})
			li++
		}
	}
	return out
}
