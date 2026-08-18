// Package agent 定义多 Agent OCR 框架的核心抽象。
package agent

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"
)

// Result 是单个 Agent 的一次识别结果。
type Result struct {
	// Agent 是产出该结果的 Agent 名称,由 Coordinator 填写。
	Agent string `json:"agent"`
	// Lines 是按阅读顺序排列的识别行,一个元素对应图中一条物理行。
	// 行是融合的最小单元:只有行级结构才能让仲裁发生在低于整篇文本的
	// 粒度上,融合准确率才有机会超过最好的单个引擎。
	//
	// 这里只有文本,不带任何引擎自报的指标。可信度由融合层从结构信号
	// (多少引擎逐字一致、多少持异议)推导,见 internal/arbiter。
	Lines []string `json:"lines,omitempty"`
	// LatencyMS 是本次识别耗时(毫秒)。
	LatencyMS int64 `json:"latency_ms"`
	// Err 为非空时表示该 Agent 识别失败,此时 Lines 无效。
	Err string `json:"err,omitempty"`
}

// Agent 是一个 OCR 引擎的抽象。实现方应按图中的物理行切分返回文本。
type Agent interface {
	// Name 返回 Agent 唯一名称,如 "tesseract"、"claude-haiku-4-5#1"。
	Name() string
	// Recognize 对输入图片执行 OCR。实现必须尊重 ctx 取消。
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

// Register 注册一个 Agent,重名时返回错误。
func (r *Registry) Register(a Agent) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.agents[a.Name()]; ok {
		return fmt.Errorf("agent %q already registered", a.Name())
	}
	r.agents[a.Name()] = a
	return nil
}

// MustRegister 注册 Agent,重名时 panic。
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

// Len 返回已注册 Agent 数量。
func (r *Registry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.agents)
}

// List 返回所有已注册 Agent,按名称字典序排列。
// 确定性顺序是整条流水线可复现的前提。
func (r *Registry) List() []Agent {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Agent, 0, len(r.agents))
	for _, a := range r.agents {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out
}

// Coordinator 并发调度多个 Agent,并收集结果。
type Coordinator struct {
	registry *Registry
}

// NewCoordinator 基于 Registry 创建调度器。
func NewCoordinator(reg *Registry) *Coordinator {
	return &Coordinator{registry: reg}
}

// RunConcurrent 并发调用所有已注册 Agent。ctx 结束时立即返回,
// 未完成的 Agent 在结果中记为 ctx 错误——即使某个 Agent 不尊重 ctx,
// 也不会拖死整条流水线(其 goroutine 会在自身返回后经缓冲通道退出,不会永久泄漏)。
// 单个 Agent 失败不影响其他 Agent,错误记录在 Result.Err 中。
func (c *Coordinator) RunConcurrent(ctx context.Context, image []byte) []Result {
	agents := c.registry.List()
	type slot struct {
		i   int
		res Result
	}
	ch := make(chan slot, len(agents))
	for i, a := range agents {
		go func(i int, a Agent) {
			start := time.Now()
			res, err := a.Recognize(ctx, image)
			res.Agent = a.Name()
			res.LatencyMS = time.Since(start).Milliseconds()
			if err != nil {
				res.Err = err.Error()
			}
			ch <- slot{i, res}
		}(i, a)
	}

	results := make([]Result, len(agents))
	done := make([]bool, len(agents))
	for range agents {
		select {
		case s := <-ch:
			results[s.i] = s.res
			done[s.i] = true
		case <-ctx.Done():
			for i, a := range agents {
				if !done[i] {
					results[i] = Result{Agent: a.Name(), Err: ctx.Err().Error()}
				}
			}
			return results
		}
	}
	return results
}
