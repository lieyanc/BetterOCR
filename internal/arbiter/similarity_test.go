package arbiter

import "testing"

func TestNormalize(t *testing.T) {
	cases := []struct{ in, want string }{
		{"  hello   world ", "hello world"},
		{"a\t b\nc", "a b c"},
		{"", ""},
		{"   ", ""},
	}
	for _, c := range cases {
		if got := Normalize(c.in); got != c.want {
			t.Errorf("Normalize(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestSimilarity(t *testing.T) {
	cases := []struct {
		a, b string
		want float64
	}{
		{"hello", "hello", 1},
		{"", "hello", 0},
		{"", "", 0},
		{"a", "a", 1},
		{"a", "b", 0},
		{"ab", "a", 0}, // 单字符无 bigram 且不相等
		{"abc", "xyz", 0},
		{"Hello,  World", "Hello, World", 1}, // 空白差异不构成分歧
		{"多引擎融合", "多引擎融合", 1},
	}
	for _, c := range cases {
		if got := Similarity(c.a, c.b); got != c.want {
			t.Errorf("Similarity(%q,%q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestSimilarityCJKPartial(t *testing.T) {
	// 一字之差的中文行:bigram 4/5 重叠,Jaccard = 3/5
	got := Similarity("多引擎融合", "多引擎融台")
	if got <= 0.5 || got >= 1 {
		t.Errorf("Similarity CJK one-char-diff = %v, want in (0.5, 1)", got)
	}
	if a, b := Similarity("x", "y"), Similarity("y", "x"); a != b {
		t.Errorf("similarity not symmetric: %v vs %v", a, b)
	}
}
