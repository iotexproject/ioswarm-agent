package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// LLM proxy — runs alongside the validator, serves OpenAI-compatible API.
// Tokens stay local on the agent machine. Gateway never sees them.

// ========================== Types ==========================

type llmMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type llmRequest struct {
	Model    string       `json:"model"`
	Messages []llmMessage `json:"messages"`
}

type llmChoice struct {
	Index        int        `json:"index"`
	Message      llmMessage `json:"message"`
	FinishReason string     `json:"finish_reason"`
}

type llmUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type llmResponse struct {
	ID      string      `json:"id"`
	Object  string      `json:"object"`
	Model   string      `json:"model"`
	Choices []llmChoice `json:"choices"`
	Usage   llmUsage    `json:"usage"`
}

// ========================== Config ==========================

var llmClient = &http.Client{Timeout: 60 * time.Second}

var anthropicModelMap = map[string]string{
	"claude-opus-4.6":   "claude-opus-4-20250514",
	"claude-sonnet-4.6": "claude-sonnet-4-20250514",
	"claude-haiku-4.5":  "claude-haiku-4-5-20251001",
}

func getClaudeKey() string {
	// 1. Check env var first (API key mode)
	if key := os.Getenv("ANTHROPIC_API_KEY"); key != "" {
		return key
	}
	// 2. Check saved OAuth token (subscription mode)
	if token, err := loadToken(); err == nil && token.AccessToken != "" {
		return token.AccessToken
	}
	return ""
}

func detectModels() []string {
	var models []string
	if getClaudeKey() != "" {
		models = append(models, "claude-opus-4.6", "claude-sonnet-4.6", "claude-haiku-4.5")
	}
	if os.Getenv("GEMINI_API_KEY") != "" {
		models = append(models, "gemini-2.0-flash", "gemini-1.5-flash")
	}
	if os.Getenv("OPENAI_API_KEY") != "" {
		models = append(models, "gpt-4o")
	}
	return models
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// ========================== Providers ==========================

func isOAuthToken(key string) bool {
	// OAuth tokens from llm setup start differently than API keys
	// API key: sk-ant-api03-xxx
	// OAuth:   starts with something else, or loaded from token file
	return !strings.HasPrefix(key, "sk-ant-api")
}

func callAnthropic(model string, messages []llmMessage) (string, error) {
	key := getClaudeKey()
	realModel := anthropicModelMap[model]
	if realModel == "" {
		realModel = model
	}

	var system string
	var apiMsgs []map[string]string
	for _, m := range messages {
		if m.Role == "system" {
			system = m.Content
		} else {
			apiMsgs = append(apiMsgs, map[string]string{"role": m.Role, "content": m.Content})
		}
	}

	body := map[string]interface{}{
		"model":      realModel,
		"max_tokens": 4096,
		"messages":   apiMsgs,
	}
	if system != "" {
		body["system"] = system
	}

	jsonBody, _ := json.Marshal(body)
	req, err := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", bytes.NewReader(jsonBody))
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}

	if isOAuthToken(key) {
		// OAuth token: use Bearer auth + Claude Code headers (like sub2api)
		req.Header.Set("authorization", "Bearer "+key)
		req.Header.Set("anthropic-beta", "claude-code-20250219,oauth-2025-04-20,interleaved-thinking-2025-05-14")
		req.Header.Set("user-agent", "claude-cli/2.1.22 (external, cli)")
		req.Header.Set("x-app", "cli")
		req.Header.Set("anthropic-dangerous-direct-browser-access", "true")
	} else {
		// API key: use x-api-key header
		req.Header.Set("x-api-key", key)
	}
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("content-type", "application/json")

	resp, err := llmClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("anthropic request: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("anthropic %d: %s", resp.StatusCode, truncate(string(respBody), 200))
	}

	var result struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", fmt.Errorf("parse anthropic response: %w", err)
	}
	if len(result.Content) == 0 {
		return "", fmt.Errorf("empty response from anthropic")
	}
	return result.Content[0].Text, nil
}

func callGemini(model string, messages []llmMessage) (string, error) {
	key := os.Getenv("GEMINI_API_KEY")
	var contents []map[string]interface{}
	for _, m := range messages {
		role := "user"
		if m.Role == "assistant" || m.Role == "model" {
			role = "model"
		}
		contents = append(contents, map[string]interface{}{
			"role":  role,
			"parts": []map[string]string{{"text": m.Content}},
		})
	}

	jsonBody, _ := json.Marshal(map[string]interface{}{"contents": contents})
	url := fmt.Sprintf("https://generativelanguage.googleapis.com/v1beta/models/%s:generateContent?key=%s", model, key)

	resp, err := llmClient.Post(url, "application/json", bytes.NewReader(jsonBody))
	if err != nil {
		return "", fmt.Errorf("gemini request: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("gemini %d: %s", resp.StatusCode, truncate(string(respBody), 200))
	}

	var result struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", fmt.Errorf("parse gemini response: %w", err)
	}
	if len(result.Candidates) == 0 || len(result.Candidates[0].Content.Parts) == 0 {
		return "", fmt.Errorf("empty response from gemini")
	}
	return result.Candidates[0].Content.Parts[0].Text, nil
}

func callOpenAI(model string, messages []llmMessage) (string, error) {
	key := os.Getenv("OPENAI_API_KEY")
	body := map[string]interface{}{"model": model, "messages": messages}
	jsonBody, _ := json.Marshal(body)

	req, err := http.NewRequest("POST", "https://api.openai.com/v1/chat/completions", bytes.NewReader(jsonBody))
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")

	resp, err := llmClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("openai request: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("openai %d: %s", resp.StatusCode, truncate(string(respBody), 200))
	}

	var result struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", fmt.Errorf("parse openai response: %w", err)
	}
	if len(result.Choices) == 0 {
		return "", fmt.Errorf("empty response from openai")
	}
	return result.Choices[0].Message.Content, nil
}

func routeLLM(model string, messages []llmMessage) (string, error) {
	switch {
	case strings.HasPrefix(model, "claude") && getClaudeKey() != "":
		return callAnthropic(model, messages)
	case strings.HasPrefix(model, "gemini") && os.Getenv("GEMINI_API_KEY") != "":
		return callGemini(model, messages)
	case strings.HasPrefix(model, "gpt") && os.Getenv("OPENAI_API_KEY") != "":
		return callOpenAI(model, messages)
	default:
		return "", fmt.Errorf("no key configured for model %s", model)
	}
}

// ========================== HTTP Handlers ==========================

func handleLLMChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, `{"error":{"message":"POST only"}}`, 405)
		return
	}

	var req llmRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]interface{}{"error": map[string]string{"message": "Invalid JSON"}})
		return
	}

	model := req.Model
	for _, prefix := range []string{"clawhive/", "google/", "anthropic/", "openai/"} {
		model = strings.TrimPrefix(model, prefix)
	}

	text, err := routeLLM(model, req.Messages)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(502)
		json.NewEncoder(w).Encode(map[string]interface{}{"error": map[string]string{"message": err.Error()}})
		return
	}

	inputTokens := 0
	for _, m := range req.Messages {
		inputTokens += len(m.Content) / 4
	}
	outputTokens := len(text) / 4

	writeJSON(w, 200, llmResponse{
		ID:     fmt.Sprintf("chatcmpl-%d", time.Now().UnixMilli()),
		Object: "chat.completion",
		Model:  model,
		Choices: []llmChoice{{
			Index:        0,
			Message:      llmMessage{Role: "assistant", Content: text},
			FinishReason: "stop",
		}},
		Usage: llmUsage{
			PromptTokens:     inputTokens,
			CompletionTokens: outputTokens,
			TotalTokens:      inputTokens + outputTokens,
		},
	})
}

func handleLLMHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]interface{}{"status": "ok", "models": detectModels()})
}

func handleLLMModels(w http.ResponseWriter, r *http.Request) {
	var data []map[string]string
	for _, m := range detectModels() {
		data = append(data, map[string]string{"id": m, "object": "model"})
	}
	writeJSON(w, 200, map[string]interface{}{"object": "list", "data": data})
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// ========================== Entry ==========================

func startLLMProxy(port int) {
	models := detectModels()
	if len(models) == 0 {
		fmt.Fprintln(os.Stderr, "No LLM keys found.")
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "Option 1: Login with Claude subscription (recommended):")
		fmt.Fprintln(os.Stderr, "  ioswarm-agent llm setup")
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "Option 2: Set API key via environment variable:")
		fmt.Fprintln(os.Stderr, "  ANTHROPIC_API_KEY=sk-xxx ioswarm-agent --mode llm")
		fmt.Fprintln(os.Stderr, "  GEMINI_API_KEY=xxx ioswarm-agent --mode llm")
		fmt.Fprintln(os.Stderr, "  OPENAI_API_KEY=sk-xxx ioswarm-agent --mode llm")
		return
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", handleLLMChat)
	mux.HandleFunc("/health", handleLLMHealth)
	mux.HandleFunc("/v1/models", handleLLMModels)

	fmt.Printf("🐝 LLM proxy on :%d — models: %v\n", port, models)

	if err := http.ListenAndServe(fmt.Sprintf(":%d", port), mux); err != nil {
		fmt.Fprintf(os.Stderr, "LLM proxy: %v\n", err)
	}
}

func runLLMProxy(args []string) {
	fs := flag.NewFlagSet("llm", flag.ExitOnError)
	port := fs.Int("port", 8082, "LLM proxy port")
	fs.Parse(args)

	fmt.Println("Tokens stay local. Gateway never sees them.")
	startLLMProxy(*port)
}
