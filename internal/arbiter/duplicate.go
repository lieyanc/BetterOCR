package arbiter

import (
	"strings"
	"unicode/utf8"
)

const (
	minDuplicateRunes = 8
	maxDuplicateGap   = 4
)

// DuplicatePair is a model-proposed relation where Later repeats Earlier.
// The proposal is only a hint; ApplyDuplicatePairs independently validates it.
type DuplicatePair struct {
	Later   int `json:"later"`
	Earlier int `json:"earlier"`
}

// ApplyDuplicatePairs conservatively removes model-proposed duplicate regions.
// Only a nearby, later disputed region can be removed. Consensus text is never
// deleted by the checker, even when the model calls it a duplicate.
func ApplyDuplicatePairs(final Final, pairs []DuplicatePair) (Final, int) {
	remove := make(map[int]struct{})
	for _, pair := range pairs {
		if !validDuplicatePair(final.Segments, pair) {
			continue
		}
		remove[pair.Later] = struct{}{}
	}
	if len(remove) == 0 {
		return final, 0
	}

	segments := make([]FinalSegment, 0, len(final.Segments)-len(remove))
	texts := make([]string, 0, cap(segments))
	confidence := 0.0
	for index, segment := range final.Segments {
		if _, dropped := remove[index]; dropped {
			continue
		}
		segments = append(segments, segment)
		texts = append(texts, segment.Text)
		confidence += segment.Confidence
	}
	final.Segments = segments
	final.Text = strings.Join(texts, "\n")
	final.Confidence = 0
	if len(segments) > 0 {
		final.Confidence = round4(confidence / float64(len(segments)))
	}
	return final, len(remove)
}

func validDuplicatePair(segments []FinalSegment, pair DuplicatePair) bool {
	if pair.Earlier < 0 || pair.Later <= pair.Earlier || pair.Later >= len(segments) ||
		pair.Later-pair.Earlier > maxDuplicateGap {
		return false
	}
	later := segments[pair.Later]
	if !later.Disputed || later.Source == SourceConsensus {
		return false
	}

	earlierText := Normalize(segments[pair.Earlier].Text)
	laterText := Normalize(later.Text)
	earlierCore := CoreNormalize(earlierText)
	laterCore := CoreNormalize(laterText)
	if utf8.RuneCountInString(laterCore) < minDuplicateRunes || earlierCore == "" {
		return false
	}
	if earlierText == laterText || strings.Contains(earlierCore, laterCore) {
		return true
	}

	earlierRunes := []rune(earlierCore)
	laterRunes := []rune(laterCore)
	longer := len(earlierRunes)
	if len(laterRunes) > longer {
		longer = len(laterRunes)
	}
	shorter := len(earlierRunes)
	if len(laterRunes) < shorter {
		shorter = len(laterRunes)
	}
	if shorter*10 < longer*9 {
		return false
	}
	maxEdits := longer / 12
	if maxEdits < 1 {
		maxEdits = 1
	}
	return editDistanceWithin(earlierRunes, laterRunes, maxEdits)
}

// editDistanceWithin computes only the diagonal band that can still satisfy
// maxDistance, keeping validation bounded for unusually long dispute regions.
func editDistanceWithin(a, b []rune, maxDistance int) bool {
	if len(a) > len(b) {
		a, b = b, a
	}
	if len(b)-len(a) > maxDistance {
		return false
	}
	inf := maxDistance + 1
	previous := make([]int, len(b)+1)
	current := make([]int, len(b)+1)
	for j := range previous {
		if j <= maxDistance {
			previous[j] = j
		} else {
			previous[j] = inf
		}
	}
	for i := 1; i <= len(a); i++ {
		for j := range current {
			current[j] = inf
		}
		if i <= maxDistance {
			current[0] = i
		}
		low := i - maxDistance
		if low < 1 {
			low = 1
		}
		high := i + maxDistance
		if high > len(b) {
			high = len(b)
		}
		for j := low; j <= high; j++ {
			cost := 0
			if a[i-1] != b[j-1] {
				cost = 1
			}
			current[j] = min3(current[j-1]+1, previous[j]+1, previous[j-1]+cost)
		}
		previous, current = current, previous
	}
	return previous[len(b)] <= maxDistance
}

func min3(a, b, c int) int {
	if a < b {
		b = a
	}
	if b < c {
		return b
	}
	return c
}
