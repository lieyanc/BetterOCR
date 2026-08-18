package agent

import (
	"context"
	"errors"
	"testing"
	"time"
)

// fakeAgent 是仅测试用的 Agent 假实现。
type fakeAgent struct {
	name      string
	lines     []string
	delay     time.Duration
	ignoreCtx bool // 模拟不尊重 ctx 的实现
	fail      error
}

func (f *fakeAgent) Name() string { return f.name }

func (f *fakeAgent) Recognize(ctx context.Context, _ []byte) (Result, error) {
	if f.delay > 0 {
		if f.ignoreCtx {
			time.Sleep(f.delay)
		} else {
			select {
			case <-time.After(f.delay):
			case <-ctx.Done():
				return Result{}, ctx.Err()
			}
		}
	}
	if f.fail != nil {
		return Result{}, f.fail
	}
	return Result{Lines: f.lines}, nil
}

func TestRegistryListSorted(t *testing.T) {
	reg := NewRegistry()
	for _, n := range []string{"c", "a", "b"} {
		reg.MustRegister(&fakeAgent{name: n})
	}
	if reg.Len() != 3 {
		t.Fatalf("Len = %d, want 3", reg.Len())
	}
	for i, want := range []string{"a", "b", "c"} {
		if got := reg.List()[i].Name(); got != want {
			t.Errorf("List()[%d] = %q, want %q", i, got, want)
		}
	}
	if _, ok := reg.Get("b"); !ok {
		t.Error("Get(b) not found")
	}
	if _, ok := reg.Get("nope"); ok {
		t.Error("Get(nope) unexpectedly found")
	}
}

func TestRegisterDuplicate(t *testing.T) {
	reg := NewRegistry()
	reg.MustRegister(&fakeAgent{name: "dup"})
	if err := reg.Register(&fakeAgent{name: "dup"}); err == nil {
		t.Error("duplicate Register should error")
	}
	defer func() {
		if recover() == nil {
			t.Error("duplicate MustRegister should panic")
		}
	}()
	reg.MustRegister(&fakeAgent{name: "dup"})
}

func TestRunConcurrentCollects(t *testing.T) {
	reg := NewRegistry()
	reg.MustRegister(&fakeAgent{name: "ok", lines: []string{"hi"}})
	reg.MustRegister(&fakeAgent{name: "sad", fail: errors.New("nope")})

	results := NewCoordinator(reg).RunConcurrent(context.Background(), nil)
	if len(results) != 2 {
		t.Fatalf("got %d results, want 2", len(results))
	}
	// List 序:ok, sad
	if results[0].Agent != "ok" || results[0].Err != "" || len(results[0].Lines) != 1 {
		t.Errorf("results[0] = %+v", results[0])
	}
	if results[1].Agent != "sad" || results[1].Err != "nope" || len(results[1].Lines) != 0 {
		t.Errorf("results[1] = %+v", results[1])
	}
}

// TestRunConcurrentEarlyReturn 验证 ctx 结束时立即返回,
// 即使某个 Agent 不尊重 ctx 也不阻塞流水线。
func TestRunConcurrentEarlyReturn(t *testing.T) {
	reg := NewRegistry()
	reg.MustRegister(&fakeAgent{name: "fast", lines: []string{"quick"}})
	reg.MustRegister(&fakeAgent{name: "stuck", delay: 2 * time.Second, ignoreCtx: true})

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	results := NewCoordinator(reg).RunConcurrent(ctx, nil)
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("RunConcurrent blocked %v, want early return around ctx deadline", elapsed)
	}
	if results[0].Agent != "fast" || results[0].Err != "" || len(results[0].Lines) != 1 {
		t.Errorf("fast agent result lost: %+v", results[0])
	}
	if results[1].Agent != "stuck" || results[1].Err != context.DeadlineExceeded.Error() {
		t.Errorf("stuck agent = %+v, want ctx deadline error", results[1])
	}
}
