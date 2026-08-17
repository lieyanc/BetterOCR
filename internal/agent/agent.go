// Package agent 定义多 Agent OCR 框架的核心抽象。
package agent

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// Result 是单个 Agent 的一次识别结果。
type Result struct {
	// Agent 是产出该结果的 Agent 名称。
	Agent string `json:"agent"`
	// Text 是识别出的完整文本。
	Text string `json:"text"`
	// Confidence 是该 Agent 对结果的置信度，范围 [0,1]。
	Confidence float64 `json:"confidence"`
	// LatencyMS 是本次识别耗时（毫秒）。
	LatencyMS int64 `json:"latency_ms"`
	// Err 为非空时表示该 Agent 识别失败，此时 Text 无效。
	Err string `json:"err,omitempty"`
}

// Agent 是一个 OCR 引擎的抽象。实现方应尽量返回带置信度的结果，
// 以便 Arbiter 进行融合决策。
type Agent interface {
	// Name 返回 Agent 唯一名称，如 "tesseract"、"paddle"。
	Name() string
	// Recognize 对输入图片执行 OCR，返回识别结果。
	Recognize(ctx context.Context, image []byte) (Result, error)
}

// Registry 负责注册与管理 Agent。
type Registry struct {
	mu     sync.RWMutex
	agents map[string]Agent
}

// NewRegistry 创建空注册表。
func NewRegistry() *Registry {
	return &Registry{agents: make(map[string]Agent)}
}

// Register 注册一个 Agent，重名时返回错误。
func (r *Registry) Register(a Agent) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.agents[a.Name()]; ok {
		return fmt.Errorf("agent %q already registered", a.Name())
	}
	r.agents[a.Name()] = a
	return nil
}

// MustRegister 注册 Agent，重名时 panic。
func (r *Registry) MustRegister(a Agent) {
	if err := r.Register(a); err != nil {
		panic(err)
	}
}

// Get 按名称查找 Agent。
func (r *Registry) Get(name string) (Agent, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	a, ok := r.agents[name]
	return a, ok
}

// List 返回所有已注册 Agent。
func (r *Registry) List() []Agent {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Agent, 0, len(r.agents))
	for _, a := range r.agents {
		out = append(out, a)
	}
	return out
}

// Coordinator 并发调度多个 Agent，并收集结果。
type Coordinator struct {
	registry *Registry
}

// NewCoordinator 基于 Registry 创建调度器。
func NewCoordinator(reg *Registry) *Coordinator {
	return &Coordinator{registry: reg}
}

// RunConcurrent 并发调用所有已注册 Agent，ctx 取消时提前返回。
// 单个 Agent 失败不影响其他 Agent，错误记录在 Result.Err 中。
func (c *Coordinator) RunConcurrent(ctx context.Context, image []byte) []Result {
	agents := c.registry.List()
	results := make([]Result, len(agents))
	var wg sync.WaitGroup
	for i, a := range agents {
		wg.Add(1)
		go func(i int, a Agent) {
			defer wg.Done()
			start := time.Now()
			res, err := a.Recognize(ctx, image)
			res.Agent = a.Name()
			res.LatencyMS = time.Since(start).Milliseconds()
			if err != nil {
				res.Err = err.Error()
			}
			results[i] = res
		}(i, a)
	}
	wg.Wait()
	return results
}
