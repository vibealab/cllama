package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// compile-time interface check
var _ Client = (*OllamaClient)(nil)

// OllamaClient talks to an Ollama server.
type OllamaClient struct {
	BaseURL    string
	Token      string // optional Bearer token
	HTTPClient *http.Client
}

// NewOllamaClient returns a client for the given base URL (e.g. http://host:11434).
// token may be empty when the server does not require auth. timeout bounds a
// whole request including body streaming (long generations need a generous
// value); 0 means no timeout.
func NewOllamaClient(baseURL, token string, timeout time.Duration) *OllamaClient {
	return &OllamaClient{
		BaseURL:    strings.TrimRight(baseURL, "/"),
		Token:      token,
		HTTPClient: &http.Client{Timeout: timeout},
	}
}

// ── Ollama wire types ────────────────────────────────────────────────────────

// OllamaChatMessage mirrors Ollama's message structure (supports images).
type OllamaChatMessage struct {
	Role      string           `json:"role"`
	Content   string           `json:"content,omitempty"`
	Thinking  string           `json:"thinking,omitempty"` // reasoning trace
	Images    []string         `json:"images,omitempty"`   // base64-encoded
	ToolCalls []ollamaToolCall `json:"tool_calls,omitempty"`
}

// ollamaToolCall mirrors Ollama's tool-call structure, where function
// arguments are a JSON object (not a JSON-encoded string like OpenAI).
type ollamaToolCall struct {
	Function ollamaFunctionCall `json:"function"`
}

type ollamaFunctionCall struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// OllamaChatRequest mirrors Ollama's /api/chat request body.
type OllamaChatRequest struct {
	Model    string              `json:"model"`
	Messages []OllamaChatMessage `json:"messages"`
	Stream   bool                `json:"stream"`
	Tools    []Tool              `json:"tools,omitempty"`
	Format   string              `json:"format,omitempty"`
	Options  map[string]any      `json:"options,omitempty"`
	Think    interface{}         `json:"think,omitempty"` // bool | level | {level}
}

// OllamaChatResponse mirrors Ollama's /api/chat response body.
type OllamaChatResponse struct {
	Model     string            `json:"model"`
	CreatedAt string            `json:"created_at"`
	Message   OllamaChatMessage `json:"message"`
	Done      bool              `json:"done"`
	Metrics   OllamaMetrics     `json:"metrics"`
}

// OllamaMetrics carries token counters reported by Ollama.
type OllamaMetrics struct {
	TotalDuration      int64 `json:"total_duration"`
	LoadDuration       int64 `json:"load_duration"`
	PromptEvalCount    int   `json:"prompt_eval_count"`
	PromptEvalDuration int64 `json:"prompt_eval_duration"`
	EvalCount          int   `json:"eval_count"`
	EvalDuration       int64 `json:"eval_duration"`
}

// ── Chat ─────────────────────────────────────────────────────────────────────

// Chat sends a non-streaming chat request to Ollama.
func (c *OllamaClient) Chat(ctx context.Context, req *ChatRequest) (*ChatResponse, error) {
	ollamaReq := c.buildChatRequest(req, false)

	resp, err := c.doJSON(ctx, "POST", "/api/chat", ollamaReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("ollama chat HTTP %d: %s", resp.StatusCode, string(body))
	}

	var ollamaResp OllamaChatResponse
	if err := json.NewDecoder(resp.Body).Decode(&ollamaResp); err != nil {
		return nil, fmt.Errorf("ollama chat decode: %w", err)
	}

	return convertOllamaChatResponse(&ollamaResp, req.Model), nil
}

// ChatStream sends a streaming chat request to Ollama and returns the raw body.
func (c *OllamaClient) ChatStream(ctx context.Context, req *ChatRequest) (io.ReadCloser, error) {
	ollamaReq := c.buildChatRequest(req, true)

	httpReq, err := c.buildRequest(ctx, "POST", "/api/chat", ollamaReq)
	if err != nil {
		return nil, err
	}

	resp, err := c.HTTPClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("ollama stream request: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("ollama stream HTTP %d: %s", resp.StatusCode, string(body))
	}
	return resp.Body, nil
}

// StreamToSSE converts Ollama's line-delimited JSON stream to OpenAI SSE.
func (c *OllamaClient) StreamToSSE(ctx context.Context, body io.Reader, model string, out chan<- string) {
	parseOllamaStream(ctx, body, model, out)
}

func (c *OllamaClient) buildChatRequest(req *ChatRequest, stream bool) OllamaChatRequest {
	return OllamaChatRequest{
		Model:    req.Model,
		Messages: convertMessagesToOllama(req.Messages),
		Stream:   stream,
		Tools:    req.Tools,
		Options:  buildOptions(req),
		Think:    req.Think,
	}
}

// ── Embeddings ───────────────────────────────────────────────────────────────

// ollamaEmbedRequest uses /api/embed which accepts a string or []string input.
type ollamaEmbedRequest struct {
	Model  string      `json:"model"`
	Input  interface{} `json:"input"`
	Stream bool        `json:"stream,omitempty"`
}

type ollamaEmbedResponse struct {
	Model      string      `json:"model"`
	Embeddings [][]float64 `json:"embeddings"`
}

// Embeddings sends a text-embedding request to Ollama.
func (c *OllamaClient) Embeddings(ctx context.Context, model string, input interface{}) ([][]float64, error) {
	embedReq := ollamaEmbedRequest{Model: model, Input: input, Stream: false}

	resp, err := c.doJSON(ctx, "POST", "/api/embed", embedReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("ollama embed HTTP %d: %s", resp.StatusCode, string(body))
	}

	var embedResp ollamaEmbedResponse
	if err := json.NewDecoder(resp.Body).Decode(&embedResp); err != nil {
		return nil, fmt.Errorf("ollama embed decode: %w", err)
	}
	if len(embedResp.Embeddings) == 0 {
		return nil, fmt.Errorf("ollama embed returned no data")
	}
	return embedResp.Embeddings, nil
}

// ── HTTP helpers ─────────────────────────────────────────────────────────────

func (c *OllamaClient) doJSON(ctx context.Context, method, path string, body any) (*http.Response, error) {
	httpReq, err := c.buildRequest(ctx, method, path, body)
	if err != nil {
		return nil, err
	}
	return c.HTTPClient.Do(httpReq)
}

func (c *OllamaClient) buildRequest(ctx context.Context, method, path string, body any) (*http.Request, error) {
	var bodyReader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		bodyReader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, bodyReader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	return req, nil
}

// ── Format conversion ────────────────────────────────────────────────────────

func convertOllamaChatResponse(ollamaResp *OllamaChatResponse, model string) *ChatResponse {
	return &ChatResponse{
		ID:      "chatcmpl-" + model + "-" + time.Now().Format("20060102150405"),
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   model,
		Choices: []ChatChoice{
			{
				Index: 0,
				Message: ChatMessage{
					Role:             "assistant",
					Content:          ollamaResp.Message.Content,
					ReasoningContent: ollamaResp.Message.Thinking,
					ToolCalls:        convertToolCalls(ollamaResp.Message.ToolCalls),
				},
				FinishReason: "stop",
			},
		},
		Usage: ChatUsage{
			PromptTokens:     ollamaResp.Metrics.PromptEvalCount,
			CompletionTokens: ollamaResp.Metrics.EvalCount,
			TotalTokens:      ollamaResp.Metrics.PromptEvalCount + ollamaResp.Metrics.EvalCount,
		},
	}
}

// convertMessagesToOllama translates OpenAI-format messages into Ollama's
// format, extracting base64 payloads from image_url data URLs into `images`.
func convertMessagesToOllama(messages []ChatMessage) []OllamaChatMessage {
	out := make([]OllamaChatMessage, len(messages))
	for i, m := range messages {
		msg := OllamaChatMessage{Role: m.Role, Thinking: m.Reasoning()}

		switch c := m.Content.(type) {
		case string:
			msg.Content = c
		case []any: // messages decoded from JSON arrive as []interface{}
			var texts []string
			for _, raw := range c {
				part, ok := raw.(map[string]any)
				if !ok {
					continue
				}
				switch part["type"] {
				case "text":
					if t, ok := part["text"].(string); ok {
						texts = append(texts, t)
					}
				case "image_url":
					if img, ok := part["image_url"].(map[string]any); ok {
						if u, ok := img["url"].(string); ok {
							if b64, isData := dataURLBase64(u); isData {
								msg.Images = append(msg.Images, b64)
							} else {
								texts = append(texts, "[image: "+u+"]")
							}
						}
					}
				}
			}
			msg.Content = strings.Join(texts, "\n")
		case []MessagePart:
			var texts []string
			for _, p := range c {
				switch p.Type {
				case "text":
					texts = append(texts, p.Text)
				case "image_url":
					if p.ImageURL != nil {
						if b64, isData := dataURLBase64(p.ImageURL.URL); isData {
							msg.Images = append(msg.Images, b64)
						} else {
							texts = append(texts, "[image: "+p.ImageURL.URL+"]")
						}
					}
				}
			}
			msg.Content = strings.Join(texts, "\n")
		case nil:
			// tool-call or assistant messages may have no content
		default:
			msg.Content = fmt.Sprintf("%v", c)
		}

		// Tool-call results (role "tool") reference the originating call id.
		if m.ToolCallID != "" {
			msg.Role = "tool"
		}
		for _, tc := range m.ToolCalls {
			// OpenAI carries arguments as a JSON string; Ollama expects an object.
			args := json.RawMessage(tc.Function.Arguments)
			if !json.Valid(args) {
				args = json.RawMessage("{}")
			}
			msg.ToolCalls = append(msg.ToolCalls, ollamaToolCall{
				Function: ollamaFunctionCall{
					Name:      tc.Function.Name,
					Arguments: args,
				},
			})
		}

		out[i] = msg
	}
	return out
}

// dataURLBase64 extracts the base64 payload from a data URL.
func dataURLBase64(u string) (string, bool) {
	if !strings.HasPrefix(u, "data:") {
		return "", false
	}
	idx := strings.Index(u, ";base64,")
	if idx == -1 {
		return "", false
	}
	return u[idx+len(";base64,"):], true
}

// convertToolCalls translates Ollama tool calls (arguments as an object) to
// OpenAI tool calls (arguments as a JSON string).
func convertToolCalls(tcs []ollamaToolCall) []ToolCall {
	if len(tcs) == 0 {
		return nil
	}
	out := make([]ToolCall, len(tcs))
	for i, tc := range tcs {
		args := string(tc.Function.Arguments)
		if !json.Valid(tc.Function.Arguments) {
			args = "{}"
		}
		out[i] = ToolCall{
			ID:   fmt.Sprintf("call_%d", i),
			Type: "function",
			Function: ToolCallFunction{
				Name:      tc.Function.Name,
				Arguments: args,
			},
		}
	}
	return out
}

func buildOptions(req *ChatRequest) map[string]any {
	opts := make(map[string]any)
	if req.Temperature != nil {
		opts["temperature"] = *req.Temperature
	}
	if req.MaxTokens > 0 {
		opts["num_predict"] = req.MaxTokens
	}
	if req.ReasoningEffort != "" {
		opts["reasoning_effort"] = req.ReasoningEffort
	}
	if len(opts) == 0 {
		return nil
	}
	return opts
}

// ── Streaming SSE parsing ───────────────────────────────────────────────────

// parseOllamaStream reads Ollama's line-delimited JSON stream and writes
// OpenAI-format SSE events into out. It closes out when done.
func parseOllamaStream(ctx context.Context, body io.Reader, model string, out chan<- string) {
	defer close(out)

	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	id := "chatcmpl-" + fmt.Sprintf("%d", time.Now().UnixNano())

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}

		var evt OllamaChatResponse
		if err := json.Unmarshal([]byte(line), &evt); err != nil {
			continue
		}

		if !evt.Done {
			choice := StreamingChatChoice{
				Index: 0,
				Delta: ChatMessage{
					Content:          evt.Message.Content,
					ReasoningContent: evt.Message.Thinking,
				},
			}
			if len(evt.Message.ToolCalls) > 0 {
				choice.Delta.ToolCalls = convertToolCalls(evt.Message.ToolCalls)
			}
			if !writeSSEEvent(ctx, out, StreamingChatResponse{
				ID:      id,
				Object:  "chat.completion.chunk",
				Created: time.Now().Unix(),
				Model:   model,
				Choices: []StreamingChatChoice{choice},
			}) {
				return
			}
		} else {
			writeSSEEvent(ctx, out, StreamingChatResponse{
				ID:      id,
				Object:  "chat.completion.chunk",
				Created: time.Now().Unix(),
				Model:   model,
				Choices: []StreamingChatChoice{
					{Index: 0, FinishReason: "stop"},
				},
				Usage: ChatUsage{
					PromptTokens:     evt.Metrics.PromptEvalCount,
					CompletionTokens: evt.Metrics.EvalCount,
					TotalTokens:      evt.Metrics.PromptEvalCount + evt.Metrics.EvalCount,
				},
			})
			select {
			case out <- "data: [DONE]\n\n":
			case <-ctx.Done():
			}
			return
		}
	}
}

// writeSSEEvent sends one SSE event, reporting false if ctx was cancelled.
func writeSSEEvent(ctx context.Context, out chan<- string, evt StreamingChatResponse) bool {
	b, err := json.Marshal(evt)
	if err != nil {
		return true // skip malformed event, keep streaming
	}
	select {
	case out <- "data: " + string(b) + "\n\n":
		return true
	case <-ctx.Done():
		return false
	}
}
