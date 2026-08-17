package pipeline

import (
	"context"
	"reflect"
	"testing"
)

func TestSplitList(t *testing.T) {
	if got := SplitList(" a, ,b ,,c "); !reflect.DeepEqual(got, []string{"a", "b", "c"}) {
		t.Errorf("SplitList = %v", got)
	}
	if got := SplitList(""); len(got) != 0 {
		t.Errorf("SplitList(\"\") = %v, want empty", got)
	}
}

func TestResolveBaseURL(t *testing.T) {
	t.Setenv("OPENAI_BASE_URL", "")
	t.Setenv("OPENAI_API_BASE", "")
	if got := ResolveBaseURL(""); got != "https://api.openai.com/v1" {
		t.Errorf("default = %q", got)
	}
	t.Setenv("OPENAI_API_BASE", "http://api-base")
	if got := ResolveBaseURL(""); got != "http://api-base" {
		t.Errorf("api-base fallback = %q", got)
	}
	t.Setenv("OPENAI_BASE_URL", "http://base-url")
	if got := ResolveBaseURL(""); got != "http://base-url" {
		t.Errorf("base-url wins over api-base = %q", got)
	}
	if got := ResolveBaseURL("http://explicit"); got != "http://explicit" {
		t.Errorf("explicit wins = %q", got)
	}
}

func TestRunRequiresEngines(t *testing.T) {
	if _, err := Run(context.Background(), Config{}, nil); err == nil {
		t.Error("Run without engines should error")
	}
}
