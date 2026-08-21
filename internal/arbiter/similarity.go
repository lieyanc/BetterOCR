package arbiter

import (
	"strings"
	"unicode"
)

// Normalize 用于判断 OCR 文本是否逐字一致。模型自行产生的换行和空白
// 不参与比较,但标点与符号保留,因为它们仍是需要裁定的可见内容。
func Normalize(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, s)
}

// CoreNormalize 去掉空白、标点和符号,只保留文字与数字。它仅用于跨模型
// 定位同一句内容;不能据此判定共识,否则金额符号、正负号等错误会被隐藏。
func CoreNormalize(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) || unicode.IsPunct(r) || unicode.IsSymbol(r) {
			return -1
		}
		return r
	}, s)
}

// Similarity 计算两段正文骨架的相似度,基于 rune bigram Jaccard。
// 中文以单字为天然单位,因此无需分词即可稳定地对齐相邻句段。
func Similarity(a, b string) float64 {
	return coreSimilarity(CoreNormalize(a), CoreNormalize(b))
}

// coreSimilarity 比较已经过 CoreNormalize 的文本。对齐动态规划会反复比较
// 同一批候选,将规整结果缓存后可避免在热路径重复扫描 Unicode 文本。
func coreSimilarity(a, b string) float64 {
	return newCoreFingerprint(a).similarity(newCoreFingerprint(b))
}

type coreFingerprint struct {
	text    string
	bigrams map[string]struct{}
}

func newCoreFingerprint(core string) coreFingerprint {
	return coreFingerprint{text: core, bigrams: bigrams(core)}
}

func (a coreFingerprint) similarity(b coreFingerprint) float64 {
	similarity, _ := a.similarityAbove(b, -1)
	return similarity
}

// similarityAbove 先用集合大小计算 Jaccard 的理论上界。上界都无法超过
// DP 当前转移的最低收益时,无需遍历较长文本的 bigram 集合。
func (a coreFingerprint) similarityAbove(b coreFingerprint, floor float64) (float64, bool) {
	if a.text == "" || b.text == "" {
		return 0, false
	}
	if a.text == b.text {
		return 1, 1 > floor
	}
	if len(a.bigrams) == 0 || len(b.bigrams) == 0 {
		return 0, false
	}
	smaller, larger := a.bigrams, b.bigrams
	if len(smaller) > len(larger) {
		smaller, larger = larger, smaller
	}
	if upper := float64(len(smaller)) / float64(len(larger)); upper <= floor {
		return 0, false
	}
	inter := 0
	for gram := range smaller {
		if _, ok := larger[gram]; ok {
			inter++
		}
	}
	similarity := float64(inter) / float64(len(a.bigrams)+len(b.bigrams)-inter)
	return similarity, similarity > floor
}

// SplitSegments 按中文句末标点切分完整 OCR 文本。换行只被当作排版空白,
// 不会制造边界;句末后的引号或括号会留在同一个句段中。
func SplitSegments(s string) []string {
	s = normalizeLayoutWhitespace(s)
	if s == "" {
		return nil
	}
	runes := []rune(s)
	segments := make([]string, 0, 8)
	start := 0
	for i := 0; i < len(runes); i++ {
		if !isSentenceEnd(runes[i]) {
			continue
		}
		end := i + 1
		for end < len(runes) && isClosingMark(runes[end]) {
			end++
		}
		if text := strings.TrimSpace(string(runes[start:end])); text != "" {
			segments = append(segments, text)
		}
		start = end
		i = end - 1
	}
	if text := strings.TrimSpace(string(runes[start:])); text != "" {
		segments = append(segments, text)
	}
	return segments
}

func normalizeLayoutWhitespace(s string) string {
	runes := []rune(strings.TrimSpace(s))
	var out []rune
	for i := 0; i < len(runes); {
		if !unicode.IsSpace(runes[i]) {
			out = append(out, runes[i])
			i++
			continue
		}
		j := i + 1
		for j < len(runes) && unicode.IsSpace(runes[j]) {
			j++
		}
		if len(out) > 0 && j < len(runes) && keepLayoutSpace(out[len(out)-1], runes[j]) {
			out = append(out, ' ')
		}
		i = j
	}
	return string(out)
}

func keepLayoutSpace(left, right rune) bool {
	return !isCJK(left) && !isCJK(right) &&
		!unicode.IsPunct(left) && !unicode.IsPunct(right) &&
		!unicode.IsSymbol(left) && !unicode.IsSymbol(right)
}

func isCJK(r rune) bool {
	return unicode.In(r, unicode.Han, unicode.Hiragana, unicode.Katakana, unicode.Hangul)
}

func isSentenceEnd(r rune) bool {
	switch r {
	case '。', '！', '？', '!', '?', '；', ';':
		return true
	default:
		return false
	}
}

func isClosingMark(r rune) bool {
	switch r {
	case '”', '’', '」', '』', '》', '）', '】', '〕', '〉', ']', ')', '"', '\'':
		return true
	default:
		return false
	}
}

func bigrams(s string) map[string]struct{} {
	set := make(map[string]struct{})
	rs := []rune(s)
	for i := 0; i+1 < len(rs); i++ {
		set[string(rs[i:i+2])] = struct{}{}
	}
	return set
}
