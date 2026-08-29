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
var _ Client = (*OpenAIClient)(nil)

// OpenAIClient talks to an OpenAI-compatible API using net/http directly.
type OpenAIClient struct {
	BaseURL    string
	Token      string // optional Bearer token
	HTTPClient *http.Client
}

// NewOpenAIClient creates a client for an OpenAI-compatible server.
// token may be empty when the server does not require auth. timeout bounds a
// whole request including body streaming (long generations need a generous
// value); 0 means no timeout.
func NewOpenAIClient(baseURL, token string, timeout time.Duration) *OpenAIClient {
	return &OpenAIClient{
		BaseURL:    strings.TrimRight(baseURL, "/"),
		Token:      token,
		HTTPClient: &http.Client{Timeout: timeout},
	}
}

// ── Chat ─────────────────────────────────────────────────────────────────────

// Chat sends a non-streaming chat request to the OpenAI-compatible API.
// Messages may contain image_url content parts for vision requests.
func (c *OpenAIClient) Chat(ctx context.Context, req *ChatRequest) (*ChatResponse, error) {
	c.normalizeThinking(req)
	httpReq, err := c.buildRequest(ctx, "POST", "/v1/chat/completions", req)
	if err != nil {
		return nil, err
	}

	resp, err := c.HTTPClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("openai chat request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("openai chat HTTP %d: %s", resp.StatusCode, string(body))
	}

	var chatResp ChatResponse
	if err := json.NewDecoder(resp.Body).Decode(&chatResp); err != nil {
		return nil, fmt.Errorf("openai chat decode: %w", err)
	}
	return &chatResp, nil
}

// ChatStream sends a streaming chat request and returns the raw SSE body.
func (c *OpenAIClient) ChatStream(ctx context.Context, req *ChatRequest) (io.ReadCloser, error) {
	c.normalizeThinking(req)
	req.Stream = true
	httpReq, err := c.buildRequest(ctx, "POST", "/v1/chat/completions", req)
	if err != nil {
		return nil, err
	}

	resp, err := c.HTTPClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("openai stream request: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("openai stream HTTP %d: %s", resp.StatusCode, string(body))
	}
	return resp.Body, nil
}

// StreamToSSE relays upstream OpenAI SSE events into out. It closes out when
// the stream completes and reports a failure when the upstream ended without
// a [DONE] event (e.g. the upstream server died mid-generation).
func (c *OpenAIClient) StreamToSSE(ctx context.Context, body io.Reader, model string, out chan<- string) error {
	defer close(out)

	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		send := "data: " + data + "\n\n"
		select {
		case out <- send:
		case <-ctx.Done():
			return ctx.Err()
		}
		if data == "[DONE]" {
			return nil
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("openai stream read: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return fmt.Errorf("openai upstream closed before [DONE]")
}

// ── Embeddings ───────────────────────────────────────────────────────────────

// Embeddings sends a text-embedding request. input may be a string or []string;
// exactly one embedding vector is returned per input item.
func (c *OpenAIClient) Embeddings(ctx context.Context, model string, input interface{}) ([][]float64, error) {
	embedReq := EmbeddingRequest{
		Model:          model,
		Input:          input,
		EncodingFormat: "float",
	}

	httpReq, err := c.buildRequest(ctx, "POST", "/v1/embeddings", embedReq)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTPClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("openai embed request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("openai embed HTTP %d: %s", resp.StatusCode, string(body))
	}

	var embedResp EmbeddingResponse
	if err := json.NewDecoder(resp.Body).Decode(&embedResp); err != nil {
		return nil, fmt.Errorf("openai embed decode: %w", err)
	}
	if len(embedResp.Data) == 0 {
		return nil, fmt.Errorf("openai embed returned no data")
	}

	embeddings := make([][]float64, len(embedResp.Data))
	for _, d := range embedResp.Data {
		if d.Index >= 0 && d.Index < len(embeddings) {
			embeddings[d.Index] = d.Embedding
		}
	}
	return embeddings, nil
}

// ── HTTP helpers ─────────────────────────────────────────────────────────

// normalizeThinking translates ollama's "think" toggle into the OpenAI-style
// reasoning_effort field and drops "think", which OpenAI-compatible upstreams
// do not understand.
func (c *OpenAIClient) normalizeThinking(req *ChatRequest) {
	if eff := req.ThinkingEffort(); eff != "" {
		req.ReasoningEffort = eff
	}
	req.Think = nil
}

func (c *OpenAIClient) buildRequest(ctx context.Context, method, path string, body any) (*http.Request, error) {
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
