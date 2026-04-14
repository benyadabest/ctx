package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/benyadabest/ctx/internal/config"
	"github.com/benyadabest/ctx/internal/store"
	bifrost "github.com/maximhq/bifrost/core"
	schemas "github.com/maximhq/bifrost/core/schemas"
)

// MODEL_COSTS: input/output price per 1M tokens (USD)
var modelCosts = map[string][2]float64{
	"claude-sonnet-4-6":           {3.00, 15.00},
	"claude-haiku-4-5-20251001":   {0.25, 1.25},
	"claude-opus-4-6":             {15.00, 75.00},
}

type Client struct {
	cfg       *config.Config
	st        *store.Store
	bifClient *bifrost.Bifrost
}

func New(cfg *config.Config, st *store.Store) (*Client, error) {
	c := &Client{cfg: cfg, st: st}

	// Only init Bifrost if we have an API key (some tasks use Ollama only)
	apiKey := cfg.API.AnthropicAPIKey
	if apiKey != "" {
		account := newAccount(apiKey)
		bc, err := bifrost.Init(context.Background(), schemas.BifrostConfig{
			Account:         account,
			InitialPoolSize: 10,
		})
		if err != nil {
			return nil, fmt.Errorf("init bifrost: %w", err)
		}
		c.bifClient = bc
	}

	return c, nil
}

// Call routes an LLM request by task to the configured model.
func (c *Client) Call(ctx context.Context, task, prompt, system string) (string, error) {
	model := c.modelForTask(task)
	if model == "" {
		return "", fmt.Errorf("no model configured for task %q", task)
	}

	start := time.Now()
	var result string
	var inputTokens, outputTokens int
	var err error

	if strings.HasPrefix(model, "ollama/") {
		ollamaModel := strings.TrimPrefix(model, "ollama/")
		result, inputTokens, outputTokens, err = c.callOllama(ctx, ollamaModel, prompt, system)
	} else {
		result, inputTokens, outputTokens, err = c.callBifrost(ctx, model, prompt, system)
	}

	if err != nil {
		return "", fmt.Errorf("llm call (%s/%s): %w", task, model, err)
	}

	// Log usage
	cost := computeCost(model, inputTokens, outputTokens)
	_ = c.st.LogUsage(&store.UsageRecord{
		TS:           start.UTC().Format(time.RFC3339),
		Task:         task,
		Model:        model,
		InputTokens:  inputTokens,
		OutputTokens: outputTokens,
		CostUSD:      cost,
	})

	return result, nil
}

func (c *Client) modelForTask(task string) string {
	switch task {
	case "summarize":
		return c.cfg.Models.Summarize
	case "detect":
		return c.cfg.Models.Detect
	case "compile":
		return c.cfg.Models.Compile
	case "synthesize":
		return c.cfg.Models.Synthesize
	default:
		return ""
	}
}

// ── Ollama ───────────────────────────────────────────────────────────────────

type ollamaRequest struct {
	Model  string `json:"model"`
	Prompt string `json:"prompt"`
	System string `json:"system,omitempty"`
	Stream bool   `json:"stream"`
}

type ollamaResponse struct {
	Response string `json:"response"`
	// Token counts may not be available from all Ollama models
	PromptEvalCount int `json:"prompt_eval_count"`
	EvalCount       int `json:"eval_count"`
}

func (c *Client) callOllama(ctx context.Context, model, prompt, system string) (string, int, int, error) {
	body, _ := json.Marshal(ollamaRequest{
		Model: model, Prompt: prompt, System: system, Stream: false,
	})

	req, err := http.NewRequestWithContext(ctx, "POST", c.cfg.API.OllamaBaseURL+"/api/generate", bytes.NewReader(body))
	if err != nil {
		return "", 0, 0, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", 0, 0, fmt.Errorf("ollama request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", 0, 0, fmt.Errorf("ollama returned %d", resp.StatusCode)
	}

	var result ollamaResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", 0, 0, fmt.Errorf("decode ollama response: %w", err)
	}

	return result.Response, result.PromptEvalCount, result.EvalCount, nil
}

// ── Bifrost (Anthropic) ──────────────────────────────────────────────────────

func ptr[T any](v T) *T { return &v }

func (c *Client) callBifrost(ctx context.Context, model, prompt, system string) (string, int, int, error) {
	if c.bifClient == nil {
		return "", 0, 0, fmt.Errorf("no API key configured — set ANTHROPIC_API_KEY")
	}

	messages := []schemas.ChatMessage{
		{
			Role:    schemas.ChatMessageRoleSystem,
			Content: &schemas.ChatMessageContent{ContentStr: ptr(system)},
		},
		{
			Role:    schemas.ChatMessageRoleUser,
			Content: &schemas.ChatMessageContent{ContentStr: ptr(prompt)},
		},
	}

	bctx := schemas.NewBifrostContext(ctx, schemas.NoDeadline)
	chatReq := &schemas.BifrostChatRequest{
		Provider: schemas.Anthropic,
		Model:    model,
		Input:    messages,
		Params: &schemas.ChatParameters{
			MaxCompletionTokens: ptr(4096),
			Temperature:         ptr(float64(0.3)),
		},
	}

	resp, bifErr := c.bifClient.ChatCompletionRequest(bctx, chatReq)
	if bifErr != nil {
		msg := "unknown error"
		if bifErr.Error != nil {
			msg = bifErr.Error.Message
		}
		return "", 0, 0, fmt.Errorf("bifrost: %s", msg)
	}

	// Extract response text
	var text string
	if len(resp.Choices) > 0 {
		choice := resp.Choices[0]
		if choice.ChatNonStreamResponseChoice != nil && choice.ChatNonStreamResponseChoice.Message != nil {
			msg := choice.ChatNonStreamResponseChoice.Message
			if msg.Content != nil && msg.Content.ContentStr != nil {
				text = *msg.Content.ContentStr
			}
		}
	}

	// Extract usage
	var inTok, outTok int
	if resp.Usage != nil {
		inTok = resp.Usage.PromptTokens
		outTok = resp.Usage.CompletionTokens
	}

	return text, inTok, outTok, nil
}

// ChatMessage represents a single message in a conversation.
type ChatMessage struct {
	Role    string `json:"role"` // "user" or "assistant"
	Content string `json:"content"`
}

// Stream sends a multi-turn chat request and streams tokens to the provided callback.
// The callback receives each text chunk as it arrives. Returns total token counts.
func (c *Client) Stream(ctx context.Context, task string, messages []ChatMessage, system string, onChunk func(string)) (int, int, error) {
	model := c.modelForTask(task)
	if model == "" {
		return 0, 0, fmt.Errorf("no model configured for task %q", task)
	}

	start := time.Now()
	var inputTokens, outputTokens int
	var err error

	if strings.HasPrefix(model, "ollama/") {
		// Ollama doesn't stream here — just call and emit as one chunk
		ollamaModel := strings.TrimPrefix(model, "ollama/")
		prompt := messages[len(messages)-1].Content
		var result string
		result, inputTokens, outputTokens, err = c.callOllama(ctx, ollamaModel, prompt, system)
		if err == nil {
			onChunk(result)
		}
	} else {
		inputTokens, outputTokens, err = c.streamBifrost(ctx, model, messages, system, onChunk)
	}

	if err != nil {
		return 0, 0, fmt.Errorf("llm stream (%s/%s): %w", task, model, err)
	}

	cost := computeCost(model, inputTokens, outputTokens)
	_ = c.st.LogUsage(&store.UsageRecord{
		TS:           start.UTC().Format(time.RFC3339),
		Task:         task,
		Model:        model,
		InputTokens:  inputTokens,
		OutputTokens: outputTokens,
		CostUSD:      cost,
	})

	return inputTokens, outputTokens, nil
}

func (c *Client) streamBifrost(ctx context.Context, model string, messages []ChatMessage, system string, onChunk func(string)) (int, int, error) {
	if c.bifClient == nil {
		return 0, 0, fmt.Errorf("no API key configured — set ANTHROPIC_API_KEY")
	}

	var bifMessages []schemas.ChatMessage
	bifMessages = append(bifMessages, schemas.ChatMessage{
		Role:    schemas.ChatMessageRoleSystem,
		Content: &schemas.ChatMessageContent{ContentStr: ptr(system)},
	})
	for _, m := range messages {
		role := schemas.ChatMessageRoleUser
		if m.Role == "assistant" {
			role = schemas.ChatMessageRoleAssistant
		}
		bifMessages = append(bifMessages, schemas.ChatMessage{
			Role:    role,
			Content: &schemas.ChatMessageContent{ContentStr: ptr(m.Content)},
		})
	}

	bctx := schemas.NewBifrostContext(ctx, schemas.NoDeadline)
	chatReq := &schemas.BifrostChatRequest{
		Provider: schemas.Anthropic,
		Model:    model,
		Input:    bifMessages,
		Params: &schemas.ChatParameters{
			MaxCompletionTokens: ptr(4096),
			Temperature:         ptr(float64(0.3)),
		},
	}

	ch, bifErr := c.bifClient.ChatCompletionStreamRequest(bctx, chatReq)
	if bifErr != nil {
		msg := "unknown error"
		if bifErr.Error != nil {
			msg = bifErr.Error.Message
		}
		return 0, 0, fmt.Errorf("bifrost stream: %s", msg)
	}

	var inTok, outTok int
	for chunk := range ch {
		if chunk.BifrostError != nil {
			continue
		}
		if chunk.BifrostChatResponse != nil {
			resp := chunk.BifrostChatResponse
			if resp.Usage != nil {
				inTok = resp.Usage.PromptTokens
				outTok = resp.Usage.CompletionTokens
			}
			for _, choice := range resp.Choices {
				if choice.ChatStreamResponseChoice != nil &&
					choice.ChatStreamResponseChoice.Delta != nil &&
					choice.ChatStreamResponseChoice.Delta.Content != nil {
					onChunk(*choice.ChatStreamResponseChoice.Delta.Content)
				}
			}
		}
	}

	return inTok, outTok, nil
}

func computeCost(model string, inputTokens, outputTokens int) float64 {
	costs, ok := modelCosts[model]
	if !ok {
		return 0
	}
	return (float64(inputTokens) * costs[0] / 1_000_000) + (float64(outputTokens) * costs[1] / 1_000_000)
}

// ── Account implementation for Bifrost ───────────────────────────────────────

type account struct {
	mu     sync.RWMutex
	apiKey string
}

func newAccount(apiKey string) *account {
	return &account{apiKey: apiKey}
}

func (a *account) GetConfiguredProviders() ([]schemas.ModelProvider, error) {
	return []schemas.ModelProvider{schemas.Anthropic}, nil
}

func (a *account) GetKeysForProvider(ctx context.Context, provider schemas.ModelProvider) ([]schemas.Key, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if provider != schemas.Anthropic {
		return nil, fmt.Errorf("unsupported provider: %s", provider)
	}
	return []schemas.Key{
		{
			ID:     "anthropic-1",
			Value:  *schemas.NewEnvVar(a.apiKey),
			Weight: 100,
		},
	}, nil
}

func (a *account) GetConfigForProvider(provider schemas.ModelProvider) (*schemas.ProviderConfig, error) {
	if provider != schemas.Anthropic {
		return nil, fmt.Errorf("unsupported provider: %s", provider)
	}
	return &schemas.ProviderConfig{
		NetworkConfig: schemas.NetworkConfig{
			BaseURL:                        "https://api.anthropic.com",
			DefaultRequestTimeoutInSeconds: 120,
			MaxRetries:                     2,
			RetryBackoffInitial:            500 * time.Millisecond,
			RetryBackoffMax:                5 * time.Second,
		},
		ConcurrencyAndBufferSize: schemas.ConcurrencyAndBufferSize{
			Concurrency: 5,
			BufferSize:  50,
		},
	}, nil
}
