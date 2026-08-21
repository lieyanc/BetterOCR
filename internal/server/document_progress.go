package server

import (
	"encoding/json"
	"math"
	"net/http"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/lieyanc/BetterOCR/internal/database"
	"github.com/lieyanc/BetterOCR/internal/pipeline"
)

const (
	documentProgressHeartbeat = time.Second
	progressPreviewRunes      = 2000
)

type documentAgentProgress struct {
	Agent           string    `json:"agent"`
	Stage           string    `json:"stage"`
	Status          string    `json:"status"`
	StartedAt       time.Time `json:"started_at"`
	ElapsedMS       int64     `json:"elapsed_ms"`
	FirstToken      bool      `json:"first_token"`
	TTFTMS          int64     `json:"ttft_ms,omitempty"`
	OutputChars     int       `json:"output_chars"`
	EstimatedTokens int       `json:"estimated_tokens"`
	TPS             float64   `json:"tps"`
	Thinking        string    `json:"thinking,omitempty"`
	Output          string    `json:"output,omitempty"`
	Error           string    `json:"error,omitempty"`
	Attempt         int       `json:"attempt"`
	MaxAttempts     int       `json:"max_attempts"`
	LastError       string    `json:"last_error,omitempty"`
	firstTokenAt    *time.Time
	firstOutputAt   *time.Time
	finishedAt      *time.Time
	outputText      string
}

type documentProgressEvent struct {
	Type             string                  `json:"type"`
	Sequence         uint64                  `json:"sequence"`
	DocumentID       string                  `json:"document_id"`
	DocumentStatus   string                  `json:"document_status"`
	PageID           string                  `json:"page_id,omitempty"`
	PageNumber       int                     `json:"page_number,omitempty"`
	Stage            string                  `json:"stage"`
	Status           string                  `json:"status"`
	StartedAt        time.Time               `json:"started_at,omitempty"`
	ElapsedMS        int64                   `json:"elapsed_ms"`
	CompletedEngines int                     `json:"completed_engines"`
	TotalEngines     int                     `json:"total_engines"`
	Agents           []documentAgentProgress `json:"agents"`
	Error            string                  `json:"error,omitempty"`
}

type documentProgressHub struct {
	mu          sync.Mutex
	sequence    uint64
	latest      map[string]documentProgressEvent
	subscribers map[string]map[chan documentProgressEvent]struct{}
}

func newDocumentProgressHub() *documentProgressHub {
	return &documentProgressHub{
		latest:      make(map[string]documentProgressEvent),
		subscribers: make(map[string]map[chan documentProgressEvent]struct{}),
	}
}

func (h *documentProgressHub) publish(progress documentProgressEvent) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.sequence++
	progress.Type = "progress"
	progress.Sequence = h.sequence
	progress = refreshedDocumentProgress(progress, time.Now())
	h.latest[progress.DocumentID] = cloneDocumentProgress(progress)
	for subscriber := range h.subscribers[progress.DocumentID] {
		update := cloneDocumentProgress(progress)
		select {
		case subscriber <- update:
		default:
			select {
			case <-subscriber:
			default:
			}
			select {
			case subscriber <- update:
			default:
			}
		}
	}
}

func (h *documentProgressHub) current(documentID string) (documentProgressEvent, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	progress, ok := h.latest[documentID]
	if !ok {
		return documentProgressEvent{}, false
	}
	return refreshedDocumentProgress(cloneDocumentProgress(progress), time.Now()), true
}

func (h *documentProgressHub) subscribe(
	documentID string,
) (chan documentProgressEvent, documentProgressEvent, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	subscriber := make(chan documentProgressEvent, 1)
	if h.subscribers[documentID] == nil {
		h.subscribers[documentID] = make(map[chan documentProgressEvent]struct{})
	}
	h.subscribers[documentID][subscriber] = struct{}{}
	progress, ok := h.latest[documentID]
	return subscriber, refreshedDocumentProgress(cloneDocumentProgress(progress), time.Now()), ok
}

func (h *documentProgressHub) unsubscribe(documentID string, subscriber chan documentProgressEvent) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.subscribers[documentID], subscriber)
	if len(h.subscribers[documentID]) == 0 {
		delete(h.subscribers, documentID)
	}
}

func (h *documentProgressHub) finishDocument(documentID, status, message string) {
	progress, ok := h.current(documentID)
	if !ok {
		progress = documentProgressEvent{
			DocumentID: documentID, StartedAt: time.Now(), Agents: []documentAgentProgress{},
		}
	}
	progress.DocumentStatus = status
	progress.Status = status
	progress.Stage = "complete"
	progress.Error = message
	h.publish(progress)
}

func (h *documentProgressHub) delete(documentID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.latest, documentID)
}

func cloneDocumentProgress(source documentProgressEvent) documentProgressEvent {
	cloned := source
	cloned.Agents = append([]documentAgentProgress(nil), source.Agents...)
	return cloned
}

func refreshedDocumentProgress(progress documentProgressEvent, now time.Time) documentProgressEvent {
	if !progress.StartedAt.IsZero() && progress.Status == "running" {
		progress.ElapsedMS = now.Sub(progress.StartedAt).Milliseconds()
	}
	completed := 0
	for index := range progress.Agents {
		agent := &progress.Agents[index]
		end := now
		if agent.finishedAt != nil {
			end = *agent.finishedAt
		}
		if !agent.StartedAt.IsZero() {
			agent.ElapsedMS = end.Sub(agent.StartedAt).Milliseconds()
		}
		if agent.firstOutputAt != nil {
			duration := end.Sub(*agent.firstOutputAt).Seconds()
			if duration >= 0.25 {
				agent.TPS = math.Round(float64(agent.EstimatedTokens)/duration*10) / 10
			}
		}
		if agent.Stage == pipeline.StageEngine && (agent.Status == "completed" || agent.Status == "failed") {
			completed++
		}
	}
	progress.CompletedEngines = completed
	return progress
}

type documentPageProgress struct {
	mu       sync.Mutex
	hub      *documentProgressHub
	progress documentProgressEvent
	closed   bool
}

func (h *documentProgressHub) beginPage(
	documentID string,
	page database.DocumentPage,
	totalEngines int,
) *documentPageProgress {
	tracker := &documentPageProgress{
		hub: h,
		progress: documentProgressEvent{
			DocumentID: documentID, DocumentStatus: database.DocumentProcessing,
			PageID: page.ID, PageNumber: page.PageNumber, Stage: "loading", Status: "running",
			StartedAt: time.Now(), TotalEngines: totalEngines, Agents: []documentAgentProgress{},
		},
	}
	h.publish(tracker.progress)
	return tracker
}

func (p *documentPageProgress) handle(event pipeline.Event) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return
	}
	now := time.Now()
	switch event.Type {
	case pipeline.EventStageStart:
		p.progress.Stage = event.Stage
		if event.Stage == pipeline.StageEngine && event.Total > 0 {
			p.progress.TotalEngines = event.Total
		}
	case pipeline.EventStageDone:
		if event.Stage == pipeline.StageEngine {
			p.progress.Stage = "merge"
		}
	case pipeline.EventAgentStart:
		p.progress.Stage = event.Stage
		agent := p.agent(event.Stage, event.Agent, now)
		agent.Status = "waiting"
		agent.MaxAttempts = event.MaxAttempts
	case pipeline.EventAttemptStart:
		p.progress.Stage = event.Stage
		agent := p.agent(event.Stage, event.Agent, now)
		if agent.Error != "" {
			agent.LastError = agent.Error
		}
		resetAgentAttempt(agent, now, event.Attempt, event.MaxAttempts)
	case pipeline.EventDelta:
		agent := p.agent(event.Stage, event.Agent, now)
		if !agent.FirstToken {
			firstTokenAt := now
			agent.FirstToken = true
			agent.firstTokenAt = &firstTokenAt
			agent.TTFTMS = now.Sub(agent.StartedAt).Milliseconds()
		}
		if event.Kind == "thinking" {
			agent.Status = "thinking"
			agent.Thinking = appendProgressPreview(agent.Thinking, event.Text)
		} else {
			agent.Status = "streaming"
			if agent.firstOutputAt == nil {
				firstOutputAt := now
				agent.firstOutputAt = &firstOutputAt
			}
			agent.OutputChars += utf8.RuneCountInString(event.Text)
			agent.outputText += event.Text
			agent.EstimatedTokens = estimateTokens(agent.outputText)
			agent.Output = appendProgressPreview(agent.Output, event.Text)
		}
	case pipeline.EventAttemptFailed:
		agent := p.agent(event.Stage, event.Agent, now)
		finishedAt := now
		agent.finishedAt = &finishedAt
		agent.ElapsedMS = event.LatencyMS
		agent.Attempt = event.Attempt
		agent.MaxAttempts = event.MaxAttempts
		agent.LastError = event.Error
		if event.Attempt < event.MaxAttempts {
			agent.Status = "retrying"
			agent.Error = ""
		} else {
			agent.Status = "failed"
			agent.Error = event.Error
		}
	case pipeline.EventAgentDone:
		agent := p.agent(event.Stage, event.Agent, now)
		finishedAt := now
		agent.finishedAt = &finishedAt
		agent.ElapsedMS = event.LatencyMS
		agent.Error = event.Error
		if event.Error != "" {
			agent.Status = "failed"
		} else {
			agent.Status = "completed"
			agent.Error = ""
		}
		if event.Stage == pipeline.StageArbiter || event.Stage == pipeline.StageDuplicateCheck {
			p.progress.Stage = "saving"
		}
	case pipeline.EventDone:
		p.progress.Stage = "saving"
	}
	p.hub.publish(p.progress)
}

func resetAgentAttempt(agent *documentAgentProgress, now time.Time, attempt, maxAttempts int) {
	agent.Status = "waiting"
	agent.StartedAt = now
	agent.ElapsedMS = 0
	agent.FirstToken = false
	agent.TTFTMS = 0
	agent.OutputChars = 0
	agent.EstimatedTokens = 0
	agent.TPS = 0
	agent.Thinking = ""
	agent.Output = ""
	agent.Error = ""
	agent.Attempt = attempt
	agent.MaxAttempts = maxAttempts
	agent.firstTokenAt = nil
	agent.firstOutputAt = nil
	agent.finishedAt = nil
	agent.outputText = ""
}

func (p *documentPageProgress) agent(stage, name string, now time.Time) *documentAgentProgress {
	for index := range p.progress.Agents {
		candidate := &p.progress.Agents[index]
		if candidate.Stage == stage && candidate.Agent == name {
			return candidate
		}
	}
	p.progress.Agents = append(p.progress.Agents, documentAgentProgress{
		Agent: name, Stage: stage, Status: "waiting", StartedAt: now,
	})
	return &p.progress.Agents[len(p.progress.Agents)-1]
}

func (p *documentPageProgress) finish(status, message string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return
	}
	p.closed = true
	p.progress.Stage = "complete"
	p.progress.Status = status
	p.progress.Error = message
	p.progress.ElapsedMS = time.Since(p.progress.StartedAt).Milliseconds()
	p.hub.publish(p.progress)
}

func appendProgressPreview(current, fragment string) string {
	runes := []rune(current + fragment)
	if len(runes) > progressPreviewRunes {
		runes = runes[len(runes)-progressPreviewRunes:]
	}
	return string(runes)
}

// Provider tokenizers differ. This estimate is deliberately exposed as an
// estimate: CJK characters count roughly one token, while ASCII text is
// grouped at approximately four non-space characters per token.
func estimateTokens(text string) int {
	tokens := 0
	ascii := 0
	flushASCII := func() {
		if ascii > 0 {
			tokens += (ascii + 3) / 4
			ascii = 0
		}
	}
	for _, character := range text {
		if character <= unicode.MaxASCII {
			if unicode.IsSpace(character) {
				flushASCII()
			} else {
				ascii++
			}
			continue
		}
		flushASCII()
		if !unicode.IsSpace(character) {
			tokens++
		}
	}
	flushASCII()
	return tokens
}

func (s *Server) handleDocumentProgress(w http.ResponseWriter, r *http.Request) {
	manager, err := s.getDocumentManager()
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	document, ok := s.ownedDocument(w, r)
	if !ok {
		return
	}

	w.Header().Set("Content-Type", "application/x-ndjson; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	encoder := json.NewEncoder(w)
	controller := http.NewResponseController(w)
	writeProgress := func(progress documentProgressEvent) bool {
		if err := encoder.Encode(progress); err != nil {
			return false
		}
		return controller.Flush() == nil
	}

	subscriber, current, exists := manager.progress.subscribe(document.ID)
	defer manager.progress.unsubscribe(document.ID, subscriber)
	if !exists {
		current = documentProgressEvent{
			Type: "progress", DocumentID: document.ID, DocumentStatus: document.Status,
			Stage: "queued", Status: document.Status, Agents: []documentAgentProgress{},
		}
	}
	if !writeProgress(current) || documentProgressTerminal(current.DocumentStatus) {
		return
	}

	ticker := time.NewTicker(documentProgressHeartbeat)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case progress := <-subscriber:
			if !writeProgress(progress) || documentProgressTerminal(progress.DocumentStatus) {
				return
			}
		case <-ticker.C:
			progress, ok := manager.progress.current(document.ID)
			if !ok {
				continue
			}
			if !writeProgress(progress) || documentProgressTerminal(progress.DocumentStatus) {
				return
			}
		}
	}
}

func documentProgressTerminal(status string) bool {
	return status == database.DocumentCompleted || status == database.DocumentFailed || status == database.DocumentCancelled
}
