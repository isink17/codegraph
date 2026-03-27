package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// OllamaLLMConfig configures the Ollama LLM provider.
type OllamaLLMConfig struct {
	BaseURL string // default: http://localhost:11434
	Model   string // default: llama3.2
}

// NewOllamaLLM returns an LLMFunc that calls a local Ollama instance.
func NewOllamaLLM(cfg OllamaLLMConfig) LLMFunc {
	if cfg.BaseURL == "" {
		cfg.BaseURL = "http://localhost:11434"
	}
	if cfg.Model == "" {
		cfg.Model = "llama3.2"
	}
	client := &http.Client{Timeout: 120 * time.Second}

	return func(ctx context.Context, prompt string) (string, error) {
		body, err := json.Marshal(map[string]any{
			"model":  cfg.Model,
			"prompt": prompt,
			"stream": false,
		})
		if err != nil {
			return "", fmt.Errorf("failed to marshal ollama request: %w", err)
		}
		req, err := http.NewRequestWithContext(ctx, "POST", cfg.BaseURL+"/api/generate", bytes.NewReader(body))
		if err != nil {
			return "", err
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			return "", fmt.Errorf("ollama LLM request failed: %w", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			b, _ := io.ReadAll(resp.Body)
			return "", fmt.Errorf("ollama returned %d: %s", resp.StatusCode, string(b))
		}
		var result struct {
			Response string `json:"response"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			return "", err
		}
		return result.Response, nil
	}
}
