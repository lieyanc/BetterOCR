// Package agents provides VLM engines and arbiters for the OpenAI Chat,
// OpenAI Responses, and Anthropic Messages APIs.
package agents

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/lieyanc/BetterOCR/internal/agent"
	"github.com/lieyanc/BetterOCR/internal/arbiter"
	"github.com/lieyanc/BetterOCR/internal/model"
)

// VisionVLM uses a configured multimodal model as a base OCR engine.
type VisionVLM struct {
	AgentName string
	Model     model.Resolved
	Client    *http.Client
	// OnDelta receives separately classified thinking and output fragments.
	OnDelta    func(StreamDelta)
	onActivity func()
}

// StreamKind separates model reasoning from text that belongs in OCR output.
type StreamKind string

const (
	StreamThinking StreamKind = "thinking"
	StreamOutput   StreamKind = "output"
)

// StreamDelta is one classified fragment from an upstream model response.
type StreamDelta struct {
	Kind StreamKind
	Text string
}

type modelOutput struct {
	Text     string
	Thinking string
}

// NewVisionVLM creates a base VLM engine.
func NewVisionVLM(name string, resolved model.Resolved, client *http.Client) *VisionVLM {
	return &VisionVLM{AgentName: name, Model: resolved, Client: client}
}

// Name implements agent.Agent.
func (v *VisionVLM) Name() string { return v.AgentName }

// SetActivityCallback lets the pipeline renew this call's idle timeout whenever
// the model emits a thinking or output fragment.
func (v *VisionVLM) SetActivityCallback(callback func()) { v.onActivity = callback }

func (v *VisionVLM) emitDelta(delta StreamDelta) {
	if v.onActivity != nil {
		v.onActivity()
	}
	if v.OnDelta != nil {
		v.OnDelta(delta)
	}
}

const ocrSystem = `You are a pure OCR engine. Transcribe every visible character in the image in natural reading order.

Return only the transcription as plain text.

Rules:
- Preserve characters exactly as printed: case, punctuation, digits, full-width/half-width forms.
- Use line breaks only when the visible layout is unambiguous; line breaks are not required.
- Do not explain, summarize, correct, infer missing text, or add markdown/code fences.
- If the image contains no text, output nothing at all.`

// Recognize implements agent.Agent.
//
// 纯文本输出没有"解析失败"这一说,代价是模型的寒暄(比如开头一句
// "Here is the transcription:")会混进行里。这里不做启发式拦截——拦不准。
// 它会成为只有一个引擎产出的孤立句段,在融合层进入仲裁,由仲裁器看图判定
// "不存在于图中"而丢弃。让架构处理噪声,比让正则去猜可靠。
func (v *VisionVLM) Recognize(ctx context.Context, image []byte) (agent.Result, error) {
	content, err := vision(ctx, v.Model, v.Client, ocrSystem, "Transcribe the image.", image, v.emitDelta)
	if err != nil {
		return agent.Result{}, err
	}
	text := strings.TrimSpace(stripFences(content))
	return agent.Result{Text: text}, nil
}

// VisionEscalator uses a configured multimodal model to resolve disputed segments.
type VisionEscalator struct {
	Model  model.Resolved
	Client *http.Client
	// OnDelta receives separately classified arbitration fragments.
	OnDelta    func(StreamDelta)
	onActivity func()
}

// NewVisionEscalator creates a dispute arbiter.
func NewVisionEscalator(resolved model.Resolved, client *http.Client) *VisionEscalator {
	return &VisionEscalator{Model: resolved, Client: client}
}

// Name implements arbiter.Escalator.
func (e *VisionEscalator) Name() string {
	return "arbiter:" + e.Model.DisplayName() + " (" + e.Model.ProviderName() + ")"
}

// SetActivityCallback lets the pipeline renew this call's idle timeout whenever
// the model emits a thinking or output fragment.
func (e *VisionEscalator) SetActivityCallback(callback func()) { e.onActivity = callback }

func (e *VisionEscalator) emitDelta(delta StreamDelta) {
	if e.onActivity != nil {
		e.onActivity()
	}
	if e.OnDelta != nil {
		e.OnDelta(delta)
	}
}

const escalatorSystem = `You are the arbiter in a multi-engine OCR system. Several OCR engines transcribed the same image. The disputed sentence segments listed below were aligned by their Chinese text content, not by physical image lines. Look at the image and decide the exact text for each segment.

Output one plain-text line per disputed segment, in exactly this form:

#<segment> <the exact text>

Rules:
- Emit exactly one line per disputed segment, reusing the segment numbers given below.
- Read the actual image; do not simply pick the most common candidate.
- Preserve punctuation and symbols exactly; they are part of the OCR result.
- If a disputed segment does not actually exist in the image, emit "#<segment>" with nothing after it.
- Do not add commentary, markdown, or code fences.`

// Resolve implements arbiter.Escalator and batches all disputed segments in one call.
func (e *VisionEscalator) Resolve(ctx context.Context, image []byte, disputes []arbiter.Dispute) ([]arbiter.Resolution, error) {
	content, err := vision(ctx, e.Model, e.Client, escalatorSystem, disputesPrompt(disputes), image, e.emitDelta)
	if err != nil {
		return nil, err
	}
	expected := make(map[int]struct{}, len(disputes))
	for _, dispute := range disputes {
		expected[dispute.Segment] = struct{}{}
	}
	parsed := make(map[int]string, len(disputes))
	for _, line := range strings.Split(stripFences(content), "\n") {
		if segment, text, ok := parseRowLine(line); ok {
			if _, exists := expected[segment]; exists {
				parsed[segment] = text
			}
		}
	}
	// 模型可能乱序、重复编号或夹带未请求的编号。批量仲裁的结果始终按
	// 输入争议顺序返回,且每个争议最多一条,便于调用方一次性稳定回填。
	out := make([]arbiter.Resolution, 0, len(parsed))
	seen := make(map[int]struct{}, len(parsed))
	for _, dispute := range disputes {
		if _, exists := seen[dispute.Segment]; exists {
			continue
		}
		text, exists := parsed[dispute.Segment]
		if !exists {
			continue
		}
		seen[dispute.Segment] = struct{}{}
		out = append(out, arbiter.Resolution{Segment: dispute.Segment, Text: text})
	}
	// 一个句段编号都认不出来是真失败,必须报错:静默返回空会让全部争议
	// 走本地兜底,而 Stats.EscalationErr 是空的——问题被藏起来了。
	if len(out) == 0 {
		return nil, fmt.Errorf("仲裁输出中没有 \"#<segment> <text>\" 形式的行: %.200s", content)
	}
	return out, nil
}

// parseRowLine 解析一行 "#<segment> <text>"。编号后必须紧跟空白或结束,
// 以免把正文里的 "#1234.5" 误读成编号;认不出的行直接忽略。
func parseRowLine(s string) (int, string, bool) {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "#") {
		return 0, "", false
	}
	rest := s[1:]
	digits := 0
	for digits < len(rest) && rest[digits] >= '0' && rest[digits] <= '9' {
		digits++
	}
	if digits == 0 {
		return 0, "", false
	}
	row, err := strconv.Atoi(rest[:digits])
	if err != nil {
		return 0, "", false
	}
	text := rest[digits:]
	if text != "" && text[0] != ' ' && text[0] != '\t' {
		return 0, "", false
	}
	return row, strings.TrimSpace(text), true
}

func disputesPrompt(disputes []arbiter.Dispute) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Disputed sentence segments (%d):\n", len(disputes))
	for _, d := range disputes {
		fmt.Fprintf(&sb, "\n#%d", d.Segment)
		if d.Before != "" || d.After != "" {
			fmt.Fprintf(&sb, " (between %q and %q)", d.Before, d.After)
		}
		sb.WriteString(":\n")
		// 候选只列文本,不列任何"票数"或"置信度"——上面刚要求它看图判断,
		// 再递上一个人气指标就是在给它反向锚点。
		for _, c := range d.Candidates {
			fmt.Fprintf(&sb, "  - engine %q read: %q\n", c.Agent, c.Text)
		}
	}
	sb.WriteString("\nReturn one \"#<segment> <text>\" line per disputed segment.")
	return sb.String()
}

func vision(ctx context.Context, resolved model.Resolved, client *http.Client, system, userText string, image []byte, onDelta func(StreamDelta)) (string, error) {
	mediaType := http.DetectContentType(image)
	if !strings.HasPrefix(mediaType, "image/") {
		return "", fmt.Errorf("输入不是可识别的图片格式: %s", mediaType)
	}
	if client == nil {
		client = http.DefaultClient
	}
	switch resolved.API {
	case model.APIOpenAIChatCompletions:
		return openAIChat(ctx, resolved, client, system, userText, mediaType, image, onDelta)
	case model.APIOpenAIResponses:
		return openAIResponses(ctx, resolved, client, system, userText, mediaType, image, onDelta)
	case model.APIAnthropicMessages:
		return anthropicMessages(ctx, resolved, client, system, userText, mediaType, image, onDelta)
	default:
		return "", fmt.Errorf("模型 %s 使用了不受支持的 API %q", resolved.Ref, resolved.API)
	}
}

type chatRequest struct {
	Model     string        `json:"model"`
	Messages  []chatMessage `json:"messages"`
	MaxTokens int           `json:"max_tokens,omitempty"`
	Stream    bool          `json:"stream"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

type chatContentPart struct {
	Type     string        `json:"type"`
	Text     string        `json:"text,omitempty"`
	ImageURL *chatImageURL `json:"image_url,omitempty"`
}

type chatImageURL struct {
	URL string `json:"url"`
}

func openAIChat(ctx context.Context, resolved model.Resolved, client *http.Client, system, userText, mediaType string, image []byte, onDelta func(StreamDelta)) (string, error) {
	body := chatRequest{
		Model: resolved.ID,
		Messages: []chatMessage{
			{Role: "system", Content: system},
			{Role: "user", Content: []chatContentPart{
				{Type: "image_url", ImageURL: &chatImageURL{URL: imageDataURL(mediaType, image)}},
				{Type: "text", Text: userText},
			}},
		},
		MaxTokens: maxOutputTokens(resolved.Context),
		Stream:    true,
	}
	resp, err := postJSON(ctx, resolved, client, "/chat/completions", body, false)
	if err != nil {
		return "", err
	}
	if isEventStream(resp) {
		return readSSEText(resp, resolved, onDelta, chatDelta)
	}
	raw, err := readResponseBody(resp)
	if err != nil {
		return "", err
	}
	output, err := parseChatResponse(raw, resolved)
	if err == nil {
		emitModelOutput(onDelta, output)
	}
	return output.Text, err
}

func parseChatResponse(raw []byte, resolved model.Resolved) (modelOutput, error) {
	var response struct {
		Choices []struct {
			Message struct {
				Content          json.RawMessage `json:"content"`
				ReasoningContent json.RawMessage `json:"reasoning_content"`
				Reasoning        json.RawMessage `json:"reasoning"`
				Thinking         json.RawMessage `json:"thinking"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		return modelOutput{}, fmt.Errorf("模型 %s 返回了无效 JSON: %w", resolved.Ref, err)
	}
	if len(response.Choices) == 0 {
		return modelOutput{}, fmt.Errorf("模型 %s 未返回任何 choice", resolved.Ref)
	}
	message := response.Choices[0].Message
	text := textFromContent(message.Content)
	if text == "" {
		return modelOutput{}, fmt.Errorf("模型 %s 未返回文本内容", resolved.Ref)
	}
	return modelOutput{
		Text:     text,
		Thinking: firstTextContent(message.ReasoningContent, message.Reasoning, message.Thinking),
	}, nil
}

type responsesRequest struct {
	Model           string             `json:"model"`
	Instructions    string             `json:"instructions"`
	Input           []responsesMessage `json:"input"`
	MaxOutputTokens int                `json:"max_output_tokens,omitempty"`
	Stream          bool               `json:"stream"`
}

type responsesMessage struct {
	Role    string                 `json:"role"`
	Content []responsesContentPart `json:"content"`
}

type responsesContentPart struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	ImageURL string `json:"image_url,omitempty"`
}

func openAIResponses(ctx context.Context, resolved model.Resolved, client *http.Client, system, userText, mediaType string, image []byte, onDelta func(StreamDelta)) (string, error) {
	body := responsesRequest{
		Model:        resolved.ID,
		Instructions: system,
		Input: []responsesMessage{{
			Role: "user",
			Content: []responsesContentPart{
				{Type: "input_image", ImageURL: imageDataURL(mediaType, image)},
				{Type: "input_text", Text: userText},
			},
		}},
		MaxOutputTokens: maxOutputTokens(resolved.Context),
		Stream:          true,
	}
	resp, err := postJSON(ctx, resolved, client, "/responses", body, false)
	if err != nil {
		return "", err
	}
	if isEventStream(resp) {
		return readSSEText(resp, resolved, onDelta, responsesDelta)
	}
	raw, err := readResponseBody(resp)
	if err != nil {
		return "", err
	}
	output, err := parseResponsesResponse(raw, resolved)
	if err == nil {
		emitModelOutput(onDelta, output)
	}
	return output.Text, err
}

func parseResponsesResponse(raw []byte, resolved model.Resolved) (modelOutput, error) {
	var response struct {
		OutputText string `json:"output_text"`
		Output     []struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
			Summary []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"summary"`
		} `json:"output"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		return modelOutput{}, fmt.Errorf("模型 %s 返回了无效 JSON: %w", resolved.Ref, err)
	}
	var textParts, thinkingParts []string
	for _, item := range response.Output {
		for _, content := range item.Content {
			if content.Type == "output_text" && content.Text != "" {
				textParts = append(textParts, content.Text)
			}
		}
		for _, summary := range item.Summary {
			if (summary.Type == "summary_text" || summary.Type == "text") && summary.Text != "" {
				thinkingParts = append(thinkingParts, summary.Text)
			}
		}
	}
	text := response.OutputText
	if strings.TrimSpace(text) == "" {
		text = strings.Join(textParts, "\n")
	}
	if text == "" {
		return modelOutput{}, fmt.Errorf("模型 %s 未返回 output_text", resolved.Ref)
	}
	return modelOutput{Text: text, Thinking: strings.Join(thinkingParts, "\n")}, nil
}

type anthropicRequest struct {
	Model     string             `json:"model"`
	System    string             `json:"system"`
	Messages  []anthropicMessage `json:"messages"`
	MaxTokens int                `json:"max_tokens"`
	Stream    bool               `json:"stream"`
}

type anthropicMessage struct {
	Role    string                 `json:"role"`
	Content []anthropicContentPart `json:"content"`
}

type anthropicContentPart struct {
	Type   string           `json:"type"`
	Text   string           `json:"text,omitempty"`
	Source *anthropicSource `json:"source,omitempty"`
}

type anthropicSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
}

func anthropicMessages(ctx context.Context, resolved model.Resolved, client *http.Client, system, userText, mediaType string, image []byte, onDelta func(StreamDelta)) (string, error) {
	body := anthropicRequest{
		Model:  resolved.ID,
		System: system,
		Messages: []anthropicMessage{{
			Role: "user",
			Content: []anthropicContentPart{
				{Type: "image", Source: &anthropicSource{
					Type: "base64", MediaType: mediaType, Data: base64.StdEncoding.EncodeToString(image),
				}},
				{Type: "text", Text: userText},
			},
		}},
		MaxTokens: maxOutputTokens(resolved.Context),
		Stream:    true,
	}
	resp, err := postJSON(ctx, resolved, client, "/messages", body, true)
	if err != nil {
		return "", err
	}
	if isEventStream(resp) {
		return readSSEText(resp, resolved, onDelta, anthropicDelta)
	}
	raw, err := readResponseBody(resp)
	if err != nil {
		return "", err
	}
	output, err := parseAnthropicResponse(raw, resolved)
	if err == nil {
		emitModelOutput(onDelta, output)
	}
	return output.Text, err
}

func parseAnthropicResponse(raw []byte, resolved model.Resolved) (modelOutput, error) {
	var response struct {
		Content []struct {
			Type     string `json:"type"`
			Text     string `json:"text"`
			Thinking string `json:"thinking"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		return modelOutput{}, fmt.Errorf("模型 %s 返回了无效 JSON: %w", resolved.Ref, err)
	}
	var textParts, thinkingParts []string
	for _, content := range response.Content {
		if content.Type == "text" && content.Text != "" {
			textParts = append(textParts, content.Text)
		}
		if content.Type == "thinking" && content.Thinking != "" {
			thinkingParts = append(thinkingParts, content.Thinking)
		}
	}
	if len(textParts) == 0 {
		return modelOutput{}, fmt.Errorf("模型 %s 未返回文本内容", resolved.Ref)
	}
	return modelOutput{
		Text: strings.Join(textParts, "\n"), Thinking: strings.Join(thinkingParts, "\n"),
	}, nil
}

func postJSON(ctx context.Context, resolved model.Resolved, client *http.Client, endpoint string, body any, anthropic bool) (*http.Response, error) {
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(resolved.BaseURL, "/")+endpoint, bytes.NewReader(encoded))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if anthropic {
		req.Header.Set("anthropic-version", "2023-06-01")
		if resolved.APIKey != "" {
			req.Header.Set("x-api-key", resolved.APIKey)
		}
	} else if resolved.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+resolved.APIKey)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		defer resp.Body.Close()
		responseBody, readErr := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
		if readErr != nil {
			return nil, readErr
		}
		message := strings.TrimSpace(string(responseBody))
		parsed := parseAPIError(responseBody)
		if parsed != "" {
			message = parsed
		}
		return nil, fmt.Errorf("模型 %s 请求失败 (HTTP %d): %.300s", resolved.Ref, resp.StatusCode, message)
	}
	return resp, nil
}

func isEventStream(resp *http.Response) bool {
	return strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream")
}

func readResponseBody(resp *http.Response) ([]byte, error) {
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if err != nil {
		return nil, err
	}
	if message := parseAPIError(body); message != "" {
		return nil, fmt.Errorf("模型返回错误: %s", message)
	}
	return body, nil
}

type sseDelta func(event string, data []byte) ([]StreamDelta, error)

func readSSEText(resp *http.Response, resolved model.Resolved, onDelta func(StreamDelta), extract sseDelta) (string, error) {
	defer resp.Body.Close()
	var out strings.Builder
	err := scanSSE(resp.Body, func(event string, data []byte) error {
		if string(data) == "[DONE]" {
			return nil
		}
		deltas, err := extract(event, data)
		if err != nil {
			return fmt.Errorf("模型 %s 返回了无效流事件: %w", resolved.Ref, err)
		}
		for _, delta := range deltas {
			if delta.Text == "" {
				continue
			}
			if delta.Kind == StreamOutput {
				out.WriteString(delta.Text)
			}
			emitDelta(onDelta, delta)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if out.Len() == 0 {
		return "", fmt.Errorf("模型 %s 未返回文本内容", resolved.Ref)
	}
	return out.String(), nil
}

func scanSSE(r io.Reader, visit func(event string, data []byte) error) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	var event string
	var data []string
	dispatch := func() error {
		if len(data) == 0 {
			event = ""
			return nil
		}
		err := visit(event, []byte(strings.Join(data, "\n")))
		event, data = "", nil
		return err
	}
	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		if line == "" {
			if err := dispatch(); err != nil {
				return err
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		field, value, _ := strings.Cut(line, ":")
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "event":
			event = value
		case "data":
			data = append(data, value)
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return dispatch()
}

func chatDelta(_ string, data []byte) ([]StreamDelta, error) {
	if message := parseAPIError(data); message != "" {
		return nil, errors.New(message)
	}
	var chunk struct {
		Choices []struct {
			Delta struct {
				Content          json.RawMessage `json:"content"`
				ReasoningContent json.RawMessage `json:"reasoning_content"`
				Reasoning        json.RawMessage `json:"reasoning"`
				Thinking         json.RawMessage `json:"thinking"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(data, &chunk); err != nil {
		return nil, err
	}
	if len(chunk.Choices) == 0 {
		return nil, nil
	}
	fragment := chunk.Choices[0].Delta
	var deltas []StreamDelta
	if thinking := firstTextContent(fragment.ReasoningContent, fragment.Reasoning, fragment.Thinking); thinking != "" {
		deltas = append(deltas, StreamDelta{Kind: StreamThinking, Text: thinking})
	}
	if text := textFromContent(fragment.Content); text != "" {
		deltas = append(deltas, StreamDelta{Kind: StreamOutput, Text: text})
	}
	return deltas, nil
}

func responsesDelta(event string, data []byte) ([]StreamDelta, error) {
	if message := parseAPIError(data); message != "" {
		return nil, errors.New(message)
	}
	var chunk struct {
		Type  string `json:"type"`
		Delta string `json:"delta"`
	}
	if err := json.Unmarshal(data, &chunk); err != nil {
		return nil, err
	}
	if chunk.Type == "response.error" || chunk.Type == "response.failed" ||
		event == "response.error" || event == "response.failed" {
		return nil, errors.New("Responses API 流返回错误")
	}
	eventType := chunk.Type
	if eventType == "" {
		eventType = event
	}
	switch eventType {
	case "response.output_text.delta":
		return []StreamDelta{{Kind: StreamOutput, Text: chunk.Delta}}, nil
	case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
		return []StreamDelta{{Kind: StreamThinking, Text: chunk.Delta}}, nil
	}
	return nil, nil
}

func anthropicDelta(event string, data []byte) ([]StreamDelta, error) {
	if message := parseAPIError(data); message != "" {
		return nil, errors.New(message)
	}
	var chunk struct {
		Type  string `json:"type"`
		Delta struct {
			Type     string `json:"type"`
			Text     string `json:"text"`
			Thinking string `json:"thinking"`
		} `json:"delta"`
	}
	if err := json.Unmarshal(data, &chunk); err != nil {
		return nil, err
	}
	if chunk.Type == "error" || event == "error" {
		return nil, errors.New("Anthropic 流返回错误")
	}
	if chunk.Type == "content_block_delta" {
		switch chunk.Delta.Type {
		case "text_delta":
			return []StreamDelta{{Kind: StreamOutput, Text: chunk.Delta.Text}}, nil
		case "thinking_delta":
			return []StreamDelta{{Kind: StreamThinking, Text: chunk.Delta.Thinking}}, nil
		}
	}
	return nil, nil
}

func emitDelta(onDelta func(StreamDelta), delta StreamDelta) {
	if onDelta != nil && delta.Text != "" {
		onDelta(delta)
	}
}

func emitModelOutput(onDelta func(StreamDelta), output modelOutput) {
	emitDelta(onDelta, StreamDelta{Kind: StreamThinking, Text: output.Thinking})
	emitDelta(onDelta, StreamDelta{Kind: StreamOutput, Text: output.Text})
}

func firstTextContent(values ...json.RawMessage) string {
	for _, value := range values {
		if text := textFromContent(value); text != "" {
			return text
		}
	}
	return ""
}

func parseAPIError(body []byte) string {
	var parsed struct {
		Type     string          `json:"type"`
		Message  string          `json:"message"`
		Error    json.RawMessage `json:"error"`
		Response *struct {
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		} `json:"response"`
	}
	if json.Unmarshal(body, &parsed) != nil {
		return ""
	}
	if len(parsed.Error) > 0 && string(parsed.Error) != "null" {
		var detail struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(parsed.Error, &detail) == nil && detail.Message != "" {
			return detail.Message
		}
		var message string
		if json.Unmarshal(parsed.Error, &message) == nil && message != "" {
			return message
		}
	}
	if parsed.Response != nil && parsed.Response.Error != nil && parsed.Response.Error.Message != "" {
		return parsed.Response.Error.Message
	}
	if (parsed.Type == "error" || strings.HasSuffix(parsed.Type, ".failed")) && parsed.Message != "" {
		return parsed.Message
	}
	return ""
}

func imageDataURL(mediaType string, image []byte) string {
	return "data:" + mediaType + ";base64," + base64.StdEncoding.EncodeToString(image)
}

func maxOutputTokens(contextWindow int) int {
	const desired = 8192
	if half := contextWindow / 2; half > 0 && half < desired {
		return half
	}
	return desired
}

func textFromContent(raw json.RawMessage) string {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	texts := make([]string, 0, len(parts))
	for _, part := range parts {
		if (part.Type == "text" || part.Type == "output_text") && part.Text != "" {
			texts = append(texts, part.Text)
		}
	}
	return strings.Join(texts, "\n")
}

// stripFences 去掉整体包裹输出的 markdown 代码围栏(```、```text 等)。
func stripFences(s string) string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "```") {
		return s
	}
	i := strings.IndexByte(s, '\n')
	if i < 0 {
		return ""
	}
	s = s[i+1:]
	if j := strings.LastIndex(s, "```"); j >= 0 {
		s = s[:j]
	}
	return strings.TrimSpace(s)
}
