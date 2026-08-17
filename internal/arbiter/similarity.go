package arbiter

import "strings"

// Normalize 将文本规整为一致性比较用的形式:去首尾空白、
// 折叠连续空白为单个空格。排版差异不构成分歧,字符差异才构成;
// 大小写与全半角差异保留——那是真实的识别分歧,应交给仲裁。
func Normalize(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// Similarity 计算两段文本的相似度,基于二元组(bigram)Jaccard 系数,
// 返回值范围 [0,1]。比较前先 Normalize;空文本与任何文本相似度为 0。
func Similarity(a, b string) float64 {
	a, b = Normalize(a), Normalize(b)
	if a == "" || b == "" {
		return 0
	}
	if a == b {
		return 1
	}
	setA, setB := bigrams(a), bigrams(b)
	if len(setA) == 0 || len(setB) == 0 {
		// 单字符文本没有 bigram;能走到这里说明两者必不相等
		return 0
	}
	inter := 0
	for g := range setA {
		if _, ok := setB[g]; ok {
			inter++
		}
	}
	return float64(inter) / float64(len(setA)+len(setB)-inter)
}

func bigrams(s string) map[string]struct{} {
	set := make(map[string]struct{})
	rs := []rune(s)
	for i := 0; i+1 < len(rs); i++ {
		set[string(rs[i:i+2])] = struct{}{}
	}
	return set
}
