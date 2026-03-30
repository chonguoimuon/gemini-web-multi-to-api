package dto

import models "gemini-web-to-api/internal/commons/models"

// ChatCompletionRequest represents OpenAI chat completion request
type ChatCompletionRequest struct {
	Model       string           `json:"model"`
	Messages    []models.Message `json:"messages"`
	Stream         bool             `json:"stream,omitempty"`
	Temperature    float32          `json:"temperature,omitempty"`
	MaxTokens      int              `json:"max_tokens,omitempty"`
	ResponseFormat *ResponseFormat  `json:"response_format,omitempty"`
	Tools          []interface{}    `json:"tools,omitempty"`
}

// ResponseFormat specifies the desired output format
type ResponseFormat struct {
	Type string `json:"type"`
}

// ChatCompletionResponse represents OpenAI chat completion response
type ChatCompletionResponse struct {
	ID                string           `json:"id"`
	Object            string           `json:"object"`
	Created           int64            `json:"created"`
	Model             string           `json:"model"`
	Choices           []Choice         `json:"choices"`
	Usage             models.Usage     `json:"usage"`
	SystemFingerprint interface{}      `json:"system_fingerprint"`
}

// Choice represents a response choice
type Choice struct {
	Index        int            `json:"index"`
	Message      models.Message `json:"message"`
	FinishReason string         `json:"finish_reason"`
}

// ChatCompletionChunk represents a streaming chunk
type ChatCompletionChunk struct {	
	ID      string        `json:"id"`
	Object  string        `json:"object"`
	Created int64         `json:"created"`
	Model   string        `json:"model"`
	Choices []ChunkChoice `json:"choices"`
}

// ChunkChoice represents a choice in a chunk
type ChunkChoice struct {
	Index        int          `json:"index"`
	Delta        models.Delta `json:"delta"`
	FinishReason string       `json:"finish_reason,omitempty"`
}

// ImageGenerationRequest represents OpenAI image generation request
type ImageGenerationRequest struct {
	Prompt         string `json:"prompt"`
	Model          string `json:"model,omitempty"`
	N              int    `json:"n,omitempty"`
	Quality        string `json:"quality,omitempty"`
	ResponseFormat string `json:"response_format,omitempty"` // "url" or "b64_json"
	Size           string `json:"size,omitempty"`
	Style          string `json:"style,omitempty"`
	User           string `json:"user,omitempty"`
}

// ImageGenerationResponse represents OpenAI image generation response
type ImageGenerationResponse struct {
	Created int64       `json:"created"`
	Data    []ImageData `json:"data"`
}

// ImageData contains the generated image URL or Base64 string
type ImageData struct {
	URL           string `json:"url,omitempty"`
	B64JSON       string `json:"b64_json,omitempty"`
	RevisedPrompt string `json:"revised_prompt,omitempty"`
}
