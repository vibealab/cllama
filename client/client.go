// Package client provides upstream LLM clients (Ollama and OpenAI-compatible
// servers) behind a common Client interface.
//
// All wire types exchanged through the interface use the OpenAI-compatible
// JSON shapes; Ollama-specific formats are translated inside OllamaClient.
package client

import (
	"context"
	"io"
)

// Client is the interface implemented by every upstream LLM backend.
type Client interface {
	// Chat sends a non-streaming chat request and returns the full response.
	Chat(ctx context.Context, req *ChatRequest) (*ChatResponse, error)

	// ChatStream sends a streaming chat request and returns the raw upstream
	// body. Use StreamToSSE to translate it into OpenAI-format SSE events.
	ChatStream(ctx context.Context, req *ChatRequest) (io.ReadCloser, error)

	// StreamToSSE reads a raw body produced by ChatStream and writes
	// OpenAI-format SSE events ("data: {...}\n\n") into out.
	// It closes out when the stream is complete and returns the stream's
	// failure: nil only when the upstream terminated cleanly ([DONE] /
	// done event). A truncated stream (upstream died, connection reset)
	// must return an error so callers do not report a silent success.
	StreamToSSE(ctx context.Context, body io.Reader, model string, out chan<- string) error

	// Embeddings returns one embedding vector per input item.
	// input may be a string or a []string.
	Embeddings(ctx context.Context, model string, input interface{}) ([][]float64, error)
}

// BackendType is the kind of upstream server.
type BackendType string

const (
	BackendOllama BackendType = "ollama"
	BackendOpenAI BackendType = "openai"
	// BackendTunnel is a reverse-tunnel backend: a child cllama server that
	// dials out to this server, receives requests over an SSE stream, and
	// POSTs results back. See server/tunnel.go.
	BackendTunnel BackendType = "tunnel"
)

// ── OpenAI-compatible chat types ─────────────────────────────────────────────

// ChatMessage mirrors OpenAI's message structure (supports images and tools).
type ChatMessage struct {
	Role     string      `json:"role"`
	Content  interface{} `json:"content,omitempty"` // string | []MessagePart
	Name     string      `json:"name,omitempty"`
	// Reasoning traces. The ecosystems name the field differently: ollama
	// uses "thinking", OpenAI-compatible servers use "reasoning_content".
	// Inbound conversions populate ReasoningContent; read it via Reasoning().
	Thinking         string        `json:"thinking,omitempty"`
	ReasoningContent string        `json:"reasoning_content,omitempty"`
	ToolCalls        []ToolCall    `json:"tool_calls,omitempty"`
	ToolCallID       string        `json:"tool_call_id,omitempty"`
}

// Reasoning returns the message's reasoning/thinking trace, whichever wire
// name it arrived under.
func (m ChatMessage) Reasoning() string {
	if m.Thinking != "" {
		return m.Thinking
	}
	return m.ReasoningContent
}

// MessagePart is a part of a multi-part message (text + image_url).
type MessagePart struct {
	Type     string    `json:"type"`
	Text     string    `json:"text,omitempty"`
	ImageURL *ImageURL `json:"image_url,omitempty"`
}

// ImageURL wraps a URL string (OpenAI expects {"url": ...}).
type ImageURL struct {
	URL string `json:"url"`
}

// ChatRequest is the /v1/chat/completions request body.
type ChatRequest struct {
	Model       string        `json:"model"`
	Messages    []ChatMessage `json:"messages"`
	Stream      bool          `json:"stream,omitempty"`
	Tools       []Tool        `json:"tools,omitempty"`
	ToolChoice  interface{}   `json:"tool_choice,omitempty"`
	Temperature *float64      `json:"temperature,omitempty"`
	MaxTokens   int           `json:"max_tokens,omitempty"`
	// Think is ollama's native thinking toggle: true|false, a level string
	// ("low"|"medium"|"high"), or {"level": "..."}. Forwarded verbatim to
	// ollama upstreams; translated to ReasoningEffort for OpenAI ones.
	Think interface{} `json:"think,omitempty"`
	// ReasoningEffort is the OpenAI-style reasoning effort level.
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
}

// ThinkingEffort resolves the ollama-style Think field to an effort level
// string ("" when unset or a plain boolean toggle).
func (r *ChatRequest) ThinkingEffort() string {
	if r.ReasoningEffort != "" {
		return r.ReasoningEffort
	}
	switch v := r.Think.(type) {
	case string:
		return v
	case map[string]interface{}:
		if lvl, ok := v["level"].(string); ok {
			return lvl
		}
	}
	return ""
}

// Tool mirrors OpenAI's tool definition.
type Tool struct {
	Type     string       `json:"type"`
	Function ToolFunction `json:"function"`
}

// ToolFunction describes a function-calling tool.
type ToolFunction struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description,omitempty"`
	Parameters  map[string]interface{} `json:"parameters,omitempty"`
}

// ToolCall is a tool call in a response message.
type ToolCall struct {
	ID       string             `json:"id"`
	Type     string             `json:"type"`
	Function ToolCallFunction   `json:"function"`
}

// ToolCallFunction describes a function invocation inside a tool call.
type ToolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// ChatChoice is a single choice inside a response.
type ChatChoice struct {
	Index        int         `json:"index"`
	Message      ChatMessage `json:"message"`
	FinishReason string      `json:"finish_reason,omitempty"`
}

// ChatUsage is token usage info.
type ChatUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// ChatResponse is the /v1/chat/completions response body.
type ChatResponse struct {
	ID      string       `json:"id"`
	Object  string       `json:"object"`
	Created int64        `json:"created"`
	Model   string       `json:"model"`
	Choices []ChatChoice `json:"choices"`
	Usage   ChatUsage    `json:"usage,omitempty"`
}

// StreamingChatChoice mirrors streaming choice deltas.
type StreamingChatChoice struct {
	Index        int         `json:"index"`
	Delta        ChatMessage `json:"delta"`
	FinishReason string      `json:"finish_reason,omitempty"`
}

// StreamingChatResponse is one SSE event body for streaming chat.
type StreamingChatResponse struct {
	ID      string                `json:"id"`
	Object  string                `json:"object"`
	Created int64                 `json:"created"`
	Model   string                `json:"model"`
	Choices []StreamingChatChoice `json:"choices"`
	Usage   ChatUsage             `json:"usage,omitempty"`
}

// ── OpenAI-compatible embedding types ────────────────────────────────────────

// EmbeddingRequest is the /v1/embeddings request body.
type EmbeddingRequest struct {
	Model          string      `json:"model"`
	Input          interface{} `json:"input"` // string | []string
	EncodingFormat string      `json:"encoding_format,omitempty"`
	User           string      `json:"user,omitempty"`
}

// EmbeddingData is embedding data per input.
type EmbeddingData struct {
	Index     int       `json:"index"`
	Object    string    `json:"object"`
	Embedding []float64 `json:"embedding"`
}

// EmbeddingUsage reports token usage for an embedding request.
type EmbeddingUsage struct {
	PromptTokens int `json:"prompt_tokens"`
	TotalTokens  int `json:"total_tokens"`
}

// EmbeddingResponse is the /v1/embeddings response body.
type EmbeddingResponse struct {
	ID      string          `json:"id"`
	Object  string          `json:"object"`
	Created int64           `json:"created"`
	Model   string          `json:"model"`
	Data    []EmbeddingData `json:"data"`
	Usage   EmbeddingUsage  `json:"usage"`
}

// ── Message constructors (image_url support) ────────────────────────────────

// NewTextMessage creates a simple text message.
func NewTextMessage(role, content string) ChatMessage {
	return ChatMessage{Role: role, Content: content}
}

// NewImageMessage creates a multi-part message with text and image URLs.
// Each URL is added as an image_url content part (OpenAI format).
func NewImageMessage(role, text string, imageURLs ...string) ChatMessage {
	parts := make([]MessagePart, 0, 1+len(imageURLs))
	if text != "" {
		parts = append(parts, MessagePart{Type: "text", Text: text})
	}
	for _, u := range imageURLs {
		parts = append(parts, MessagePart{
			Type:     "image_url",
			ImageURL: &ImageURL{URL: u},
		})
	}
	return ChatMessage{Role: role, Content: parts}
}

// NewImageMessageFromBase64 creates a multi-part message with text and a
// base64-encoded image in OpenAI's data-URL format.
func NewImageMessageFromBase64(role, text, mimeType, base64Data string) ChatMessage {
	return NewImageMessage(role, text, "data:"+mimeType+";base64,"+base64Data)
}
