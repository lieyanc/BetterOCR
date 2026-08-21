// Package pipeline 把引擎组装、并发识别与融合串成一次完整调用。
// CLI 与 Web 服务复用同一条流水线:浏览器里的一次识别与命令行
// 的一次调用在行为上完全等价。
package pipeline

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/lieyanc/BetterOCR/internal/agent"
	"github.com/lieyanc/BetterOCR/internal/agents"
	"github.com/lieyanc/BetterOCR/internal/arbiter"
	"github.com/lieyanc/BetterOCR/internal/model"
)

// Config 描述一次识别的全部参数。
type Config struct {
	// Engines are fully resolved provider models. Repeats enable multi-sampling.
	Engines []model.Resolved
	// Arbiter is nil when disputed segments should use the local candidate.
	Arbiter *model.Resolved
	// DeferArbitration leaves disputes unresolved for user merge or a later call.
	DeferArbitration bool
	// HTTPClient 为 nil 时使用 http.DefaultClient。
	HTTPClient *http.Client
	// EngineTimeout is the maximum idle period for every engine attempt.
	EngineTimeout time.Duration
	// ArbiterTimeout is the maximum idle period for every arbitration attempt.
	ArbiterTimeout time.Duration
	// EngineMaxAttempts is per engine; one means no retry.
	EngineMaxAttempts int
	// ArbiterMaxAttempts applies only to arbitration; one means no retry.
	ArbiterMaxAttempts int
	// OnDelta receives serialized model text fragments while engines and the
	// optional arbiter are running. Nil disables progress reporting.
	OnDelta func(Delta)
	// OnEvent receives model lifecycle and text events. Lifecycle events make
	// the engine-completion barrier before arbitration observable.
	OnEvent func(Event)
}

// Delta is one streamed text fragment from a model call.
type Delta struct {
	Stage string `json:"stage"`
	Agent string `json:"agent"`
	Kind  string `json:"kind"`
	Text  string `json:"text"`
}

// Event describes one observable pipeline transition.
type Event struct {
	Type        string `json:"type"`
	Stage       string `json:"stage"`
	Agent       string `json:"agent,omitempty"`
	Kind        string `json:"kind,omitempty"`
	Text        string `json:"text,omitempty"`
	Total       int    `json:"total,omitempty"`
	LatencyMS   int64  `json:"latency_ms,omitempty"`
	Attempt     int    `json:"attempt,omitempty"`
	MaxAttempts int    `json:"max_attempts,omitempty"`
	Error       string `json:"error,omitempty"`
}

const (
	StageEngine  = "engine"
	StageArbiter = "arbiter"

	EventStageStart    = "stage_start"
	EventStageDone     = "stage_done"
	EventAgentStart    = "agent_start"
	EventAgentDone     = "agent_done"
	EventAttemptStart  = "attempt_start"
	EventAttemptFailed = "attempt_failed"
	EventDelta         = "delta"
	EventDone          = "done"
)

type observedAgent struct {
	agent.Agent
	timeout     time.Duration
	maxAttempts int
	emit        func(Event)
}

type activityAware interface {
	SetActivityCallback(func())
}

func (a observedAgent) Recognize(ctx context.Context, image []byte) (agent.Result, error) {
	started := time.Now()
	maxAttempts := normalizeAttempts(a.maxAttempts)
	a.emit(Event{
		Type: EventAgentStart, Stage: StageEngine, Agent: a.Name(), MaxAttempts: maxAttempts,
	})
	var result agent.Result
	var err error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if parentErr := ctx.Err(); parentErr != nil {
			err = parentErr
			break
		}
		a.emit(Event{
			Type: EventAttemptStart, Stage: StageEngine, Agent: a.Name(),
			Attempt: attempt, MaxAttempts: maxAttempts,
		})
		attemptStarted := time.Now()
		attemptCtx, cancel, touch := withIdleTimeout(ctx, a.timeout)
		setActivityCallback(a.Agent, touch)
		result, err = a.Agent.Recognize(attemptCtx, image)
		setActivityCallback(a.Agent, nil)
		err = idleTimeoutError(ctx, attemptCtx, a.timeout, err)
		cancel()
		if err == nil {
			break
		}
		a.emit(Event{
			Type: EventAttemptFailed, Stage: StageEngine, Agent: a.Name(),
			Attempt: attempt, MaxAttempts: maxAttempts,
			LatencyMS: time.Since(attemptStarted).Milliseconds(), Error: err.Error(),
		})
		if ctx.Err() != nil {
			err = ctx.Err()
			break
		}
	}
	event := Event{
		Type: EventAgentDone, Stage: StageEngine, Agent: a.Name(),
		LatencyMS: time.Since(started).Milliseconds(), MaxAttempts: maxAttempts,
	}
	if err != nil {
		event.Error = err.Error()
	}
	a.emit(event)
	return result, err
}

type observedEscalator struct {
	arbiter.Escalator
	timeout     time.Duration
	maxAttempts int
	emit        func(Event)
}

func (e observedEscalator) Resolve(
	ctx context.Context,
	image []byte,
	disputes []arbiter.Dispute,
) ([]arbiter.Resolution, error) {
	return ResolveWithRetry(ctx, e.Escalator, image, disputes, e.timeout, e.maxAttempts, e.emit)
}

// ResolveWithRetry runs arbitration with a fresh idle timeout for every attempt.
// It is shared by automatic arbitration and the manual arbitration endpoint.
func ResolveWithRetry(
	ctx context.Context,
	escalator arbiter.Escalator,
	image []byte,
	disputes []arbiter.Dispute,
	timeout time.Duration,
	maxAttempts int,
	emit func(Event),
) ([]arbiter.Resolution, error) {
	if emit == nil {
		emit = func(Event) {}
	}
	started := time.Now()
	maxAttempts = normalizeAttempts(maxAttempts)
	emit(Event{
		Type: EventAgentStart, Stage: StageArbiter, Agent: escalator.Name(), MaxAttempts: maxAttempts,
	})
	var resolutions []arbiter.Resolution
	var err error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if parentErr := ctx.Err(); parentErr != nil {
			err = parentErr
			break
		}
		emit(Event{
			Type: EventAttemptStart, Stage: StageArbiter, Agent: escalator.Name(),
			Attempt: attempt, MaxAttempts: maxAttempts,
		})
		attemptStarted := time.Now()
		attemptCtx, cancel, touch := withIdleTimeout(ctx, timeout)
		setActivityCallback(escalator, touch)
		resolutions, err = escalator.Resolve(attemptCtx, image, disputes)
		setActivityCallback(escalator, nil)
		err = idleTimeoutError(ctx, attemptCtx, timeout, err)
		cancel()
		if err == nil {
			break
		}
		emit(Event{
			Type: EventAttemptFailed, Stage: StageArbiter, Agent: escalator.Name(),
			Attempt: attempt, MaxAttempts: maxAttempts,
			LatencyMS: time.Since(attemptStarted).Milliseconds(), Error: err.Error(),
		})
		if ctx.Err() != nil {
			err = ctx.Err()
			break
		}
	}
	event := Event{
		Type: EventAgentDone, Stage: StageArbiter, Agent: escalator.Name(),
		LatencyMS: time.Since(started).Milliseconds(), MaxAttempts: maxAttempts,
	}
	if err != nil {
		event.Error = err.Error()
	}
	emit(event)
	return resolutions, err
}

func normalizeTimeout(timeout time.Duration) time.Duration {
	if timeout > 0 {
		return timeout
	}
	return 2 * time.Minute
}

func setActivityCallback(target any, callback func()) {
	if aware, ok := target.(activityAware); ok {
		aware.SetActivityCallback(callback)
	}
}

func withIdleTimeout(parent context.Context, timeout time.Duration) (
	context.Context,
	context.CancelFunc,
	func(),
) {
	timeout = normalizeTimeout(timeout)
	ctx, cancelCause := context.WithCancelCause(parent)
	activity := make(chan struct{}, 1)
	var activityMu sync.Mutex
	lastActivity := time.Now()
	touch := func() {
		activityMu.Lock()
		lastActivity = time.Now()
		activityMu.Unlock()
		select {
		case activity <- struct{}{}:
		default:
		}
	}

	go func() {
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-activity:
			case <-timer.C:
			}

			activityMu.Lock()
			remaining := timeout - time.Since(lastActivity)
			activityMu.Unlock()
			if remaining <= 0 {
				cancelCause(context.DeadlineExceeded)
				return
			}
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(remaining)
		}
	}()

	return ctx, func() { cancelCause(context.Canceled) }, touch
}

func idleTimeoutError(parent, attempt context.Context, timeout time.Duration, err error) error {
	if err != nil && parent.Err() == nil && errors.Is(context.Cause(attempt), context.DeadlineExceeded) {
		return fmt.Errorf("连续 %s 未收到模型输出: %w", normalizeTimeout(timeout), context.DeadlineExceeded)
	}
	return err
}

func normalizeAttempts(maxAttempts int) int {
	if maxAttempts > 0 {
		return maxAttempts
	}
	return 1
}

// Run 执行一次完整识别:并发全文 OCR → 中文句段对齐 → 共识/仲裁融合。
func Run(ctx context.Context, cfg Config, image []byte) (arbiter.Final, error) {
	if len(cfg.Engines) == 0 {
		return arbiter.Final{}, errors.New("未配置任何引擎模型")
	}

	reg := agent.NewRegistry()
	var eventMu sync.Mutex
	currentAttempts := make(map[string]Event)
	emit := func(event Event) {
		// Engine calls run concurrently. Serialize every callback so consumers
		// can treat lifecycle ordering as a reliable arbitration barrier.
		eventMu.Lock()
		defer eventMu.Unlock()
		key := event.Stage + "\x00" + event.Agent
		if event.Type == EventAttemptStart {
			currentAttempts[key] = event
		} else if event.Type == EventDelta {
			if current, ok := currentAttempts[key]; ok {
				event.Attempt = current.Attempt
				event.MaxAttempts = current.MaxAttempts
			}
		}
		if event.Type == EventDelta && event.Text != "" && cfg.OnDelta != nil {
			cfg.OnDelta(Delta{
				Stage: event.Stage, Agent: event.Agent, Kind: event.Kind, Text: event.Text,
			})
		}
		if cfg.OnEvent != nil {
			cfg.OnEvent(event)
		}
	}
	emit(Event{Type: EventStageStart, Stage: StageEngine, Total: len(cfg.Engines)})
	for i, resolved := range cfg.Engines {
		// Provider and sequence keep names unique even when aliases are repeated.
		name := fmt.Sprintf("%s · %s#%d", resolved.DisplayName(), resolved.ProviderName(), i+1)
		vlm := agents.NewVisionVLM(name, resolved, cfg.HTTPClient)
		vlm.OnDelta = func(delta agents.StreamDelta) {
			emit(Event{
				Type: EventDelta, Stage: StageEngine, Agent: name,
				Kind: string(delta.Kind), Text: delta.Text,
			})
		}
		reg.MustRegister(observedAgent{
			Agent: vlm, timeout: cfg.EngineTimeout,
			maxAttempts: cfg.EngineMaxAttempts, emit: emit,
		})
	}

	arb := arbiter.New()
	arb.DeferEscalation = cfg.DeferArbitration
	if cfg.Arbiter != nil {
		escalator := agents.NewVisionEscalator(*cfg.Arbiter, cfg.HTTPClient)
		escalator.OnDelta = func(delta agents.StreamDelta) {
			emit(Event{
				Type: EventDelta, Stage: StageArbiter, Agent: escalator.Name(),
				Kind: string(delta.Kind), Text: delta.Text,
			})
		}
		arb.Escalator = observedEscalator{
			Escalator: escalator, timeout: cfg.ArbiterTimeout,
			maxAttempts: cfg.ArbiterMaxAttempts, emit: emit,
		}
	}

	results := agent.NewCoordinator(reg).RunConcurrent(ctx, image)
	emit(Event{Type: EventStageDone, Stage: StageEngine, Total: len(results)})
	final := arb.Fuse(ctx, image, results)
	emit(Event{Type: EventDone})
	return final, nil
}

// SplitList 把逗号分隔的模型列表拆成去空白的非空项。
func SplitList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}
