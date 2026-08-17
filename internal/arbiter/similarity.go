package arbiter

import "strings"

// Similarity 计算两个字符串的相似度，基于二元组（bigram）Jaccard 系数，
// 返回值范围 [0,1]。空字符串与任意字符串相似度为 0。
func Similarity(a, b string) float64 {
	a, b = strings.TrimSpace(a), strings.TrimSpace(b)
	if a == "" || b == "" {
		return 0
	}
	if a == b {
		return 1
	}
	setA := bigrams(a)
	setB := bigrams(b)
	if len(setA) == 0 || len(setB) == 0 {
		// 单字符字符串没有 bigram，退化为逐字符比较
		if len([]rune(a)) == 1 && len([]rune(b)) == 1 {
			if a == b {
				return 1
			}
		}
		return 0
	}
	inter := 0
	for g := range setA {
		if _, ok := setB[g]; ok {
			inter++
		}
	}
	union := len(setA) + len(setB) - inter
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}

func bigrams(s string) map[string]struct{} {
	set := make(map[string]struct{})
	rs := []rune(s)
	for i := 0; i+1 < len(rs); i++ {
		set[string(rs[i:i+2])] = struct{}{}
	}
	return set
}
