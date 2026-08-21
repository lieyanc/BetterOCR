package agents

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/lieyanc/BetterOCR/internal/arbiter"
	"github.com/lieyanc/BetterOCR/internal/model"
)

// DuplicateChecker asks a fast text model to nominate duplicate final regions.
// It never edits OCR text; the arbiter package validates every proposed pair.
type DuplicateChecker struct {
	Model      model.Resolved
	Client     *http.Client
	OnDelta    func(StreamDelta)
	onActivity func()
}

func NewDuplicateChecker(resolved model.Resolved, client *http.Client) *DuplicateChecker {
	return &DuplicateChecker{Model: resolved, Client: client}
}

func (c *DuplicateChecker) Name() string {
	return "fast-model:" + c.Model.DisplayName() + " (" + c.Model.ProviderName() + ")"
}

func (c *DuplicateChecker) SetActivityCallback(callback func()) { c.onActivity = callback }

func (c *DuplicateChecker) emitDelta(delta StreamDelta) {
	if c.onActivity != nil {
		c.onActivity()
	}
	if c.OnDelta != nil {
		c.OnDelta(delta)
	}
}

const duplicateCheckerSystem = `You are the final duplicate detector in an OCR fusion pipeline. Inspect the numbered final text regions and report only regions that were accidentally repeated by merging.

Output either NONE, or one plain-text line per duplicate in exactly this form:

#<later> -> #<earlier>

Rules:
- The later region must repeat the complete meaning and visible text of the earlier region.
- Report only accidental merge duplication, not headings, labels, clauses, or other text that legitimately recurs in the document.
- The later region number must be greater than the earlier region number.
- Do not rewrite, correct, quote, summarize, or merge OCR text.
- Do not add commentary, markdown, or code fences.`

func (c *DuplicateChecker) Check(ctx context.Context, segments []arbiter.FinalSegment) ([]arbiter.DuplicatePair, error) {
	content, err := textCompletion(ctx, c.Model, c.Client, duplicateCheckerSystem, duplicatePrompt(segments), c.emitDelta)
	if err != nil {
		return nil, err
	}
	plain := strings.TrimSpace(stripFences(content))
	if strings.EqualFold(plain, "NONE") {
		return []arbiter.DuplicatePair{}, nil
	}

	pairs := make([]arbiter.DuplicatePair, 0)
	seen := make(map[int]struct{})
	for _, line := range strings.Split(plain, "\n") {
		pair, ok := parseDuplicateLine(line)
		if !ok {
			continue
		}
		if _, exists := seen[pair.Later]; exists {
			continue
		}
		seen[pair.Later] = struct{}{}
		pairs = append(pairs, pair)
	}
	if len(pairs) == 0 {
		return nil, fmt.Errorf("重复检查输出中没有 NONE 或 \"#<later> -> #<earlier>\" 形式的行: %.200s", content)
	}
	return pairs, nil
}

func duplicatePrompt(segments []arbiter.FinalSegment) string {
	var prompt strings.Builder
	fmt.Fprintf(&prompt, "Final OCR text regions (%d):\n", len(segments))
	for index, segment := range segments {
		fmt.Fprintf(&prompt, "#%d %q\n", index, segment.Text)
	}
	prompt.WriteString("\nReturn duplicate mappings only, or NONE.")
	return prompt.String()
}

func parseDuplicateLine(line string) (arbiter.DuplicatePair, bool) {
	fields := strings.Fields(strings.TrimSpace(line))
	if len(fields) != 3 || fields[1] != "->" {
		return arbiter.DuplicatePair{}, false
	}
	later, ok := parseHashIndex(fields[0])
	if !ok {
		return arbiter.DuplicatePair{}, false
	}
	earlier, ok := parseHashIndex(fields[2])
	if !ok {
		return arbiter.DuplicatePair{}, false
	}
	return arbiter.DuplicatePair{Later: later, Earlier: earlier}, true
}

func parseHashIndex(value string) (int, bool) {
	if len(value) < 2 || value[0] != '#' {
		return 0, false
	}
	for _, character := range value[1:] {
		if character < '0' || character > '9' {
			return 0, false
		}
	}
	index, err := strconv.Atoi(value[1:])
	return index, err == nil
}
