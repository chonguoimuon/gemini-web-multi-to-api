package models

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Message represents a chat message (shared across OpenAI, Claude, etc)
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

// ToolCall represents an OpenAI tool call
type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function FunctionCall `json:"function"`
}

// FunctionCall represents the function details of a tool call
type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// UnmarshalJSON handles both string and array (multimodal) content
func (m *Message) UnmarshalJSON(data []byte) error {
	type Alias Message
	var aux struct {
		*Alias
		Content interface{} `json:"content"`
	}
	aux.Alias = (*Alias)(m)

	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}

	switch v := aux.Content.(type) {
	case string:
		m.Content = v
	case []interface{}:
		var textContent strings.Builder
		for _, item := range v {
			if obj, ok := item.(map[string]interface{}); ok {
				if typ, valid := obj["type"].(string); !valid || typ == "text" {
					if text, valid := obj["text"].(string); valid {
						textContent.WriteString(text)
						textContent.WriteString("\n")
					}
				}
			}
		}
		m.Content = strings.TrimSpace(textContent.String())
	case nil:
		m.Content = ""
	default:
		return fmt.Errorf("unsupported content type in message: %T", v)
	}

	return nil
}

// ModelListResponse represents the list of models
type ModelListResponse struct {
	Object string      `json:"object,omitempty"`
	Data   []ModelData `json:"data"`
}

// ModelData represents a single model in the list
type ModelData struct {
	ID          string `json:"id"`
	Object      string `json:"object,omitempty"`
	Type        string `json:"type,omitempty"`
	Created     int64  `json:"created,omitempty"`
	CreatedAt   int64  `json:"created_at,omitempty"`
	OwnedBy     string `json:"owned_by,omitempty"`
	DisplayName string `json:"display_name,omitempty"`
}

// Delta represents the delta content in a chunk
type Delta struct {
	Type      string      `json:"type,omitempty"`    // "text_delta"
	Content   string      `json:"content,omitempty"` // for OpenAI
	Text      string      `json:"text,omitempty"`    // for Claude
	Role      string      `json:"role,omitempty"`
	ToolCalls []ToolCall  `json:"tool_calls,omitempty"`
}

// Usage represents token usage (compatible format)
type Usage struct {
	PromptTokens             int                  `json:"prompt_tokens"`
	CompletionTokens         int                  `json:"completion_tokens"`
	TotalTokens              int                  `json:"total_tokens"`
	InputTokens      		 int 				  `json:"input_tokens,omitempty"`
	OutputTokens             int 				  `json:"output_tokens,omitempty"`	
	CompletionTokensDetails  interface{}   		  `json:"completion_tokens_details"`
	PromptTokensDetails      interface{}       	  `json:"prompt_tokens_details"`
}

type CompletionDetails struct {
	TextTokens      int `json:"text_tokens"`
	ReasoningTokens int `json:"reasoning_tokens"`
}

type PromptDetails struct {
	TextTokens int `json:"text_tokens"`
}

// ErrorResponse represents a standard error response
type ErrorResponse struct {
	Error interface{} `json:"error,omitempty"` // Can be string or map[string]interface{}
	Code  string      `json:"code,omitempty"`
	Type  string      `json:"type,omitempty"`
}

// Error represents error details (legacy struct format)
type Error struct {
	Message string `json:"message"`
	Type    string `json:"type,omitempty"`
	Code    string `json:"code,omitempty"`
}

// EmbeddingsRequest represents a request for embeddings
type EmbeddingsRequest struct {
	Input interface{} `json:"input"`
	Model string      `json:"model"`
}

// EmbeddingsResponse represents embeddings response
type EmbeddingsResponse struct {
	Object string        `json:"object"`
	Data   []Embedding   `json:"data"`
	Model  string        `json:"model"`
	Usage  Usage         `json:"usage"`
}

// Embedding represents a single embedding
type Embedding struct {
	Object    string    `json:"object"`
	Index     int       `json:"index"`
	Embedding []float32 `json:"embedding"`
}
