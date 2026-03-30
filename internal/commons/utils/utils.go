package utils

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"gemini-web-to-api/internal/commons/models"

	"go.uber.org/zap"
)

// BuildPromptFromMessages constructs a unified prompt from messages
func BuildPromptFromMessages(messages []models.Message, systemPrompt string) string {
	var promptBuilder strings.Builder

	if systemPrompt != "" {
		promptBuilder.WriteString(fmt.Sprintf("System: %s\n\n", systemPrompt))
	}

	for _, msg := range messages {
		if strings.EqualFold(msg.Role, "system") {
			promptBuilder.WriteString(fmt.Sprintf("System: %s\n", msg.Content))
			continue
		}

		if strings.EqualFold(msg.Role, "tool") {
			promptBuilder.WriteString(fmt.Sprintf("Tool Result [%s]:\n%s\n", msg.ToolCallID, msg.Content))
			continue
		}

		role := "User"
		if strings.EqualFold(msg.Role, "assistant") || strings.EqualFold(msg.Role, "model") {
			role = "Model"
		}

		if len(msg.ToolCalls) > 0 {
			toolsBytes, _ := json.Marshal(msg.ToolCalls)
			promptBuilder.WriteString(fmt.Sprintf("%s: Executed Tool Action: %s\n", role, string(toolsBytes)))
		} else {
			promptBuilder.WriteString(fmt.Sprintf("%s: %s\n", role, msg.Content))
		}
	}

	return strings.TrimSpace(promptBuilder.String())
}

// ValidateMessages validates that messages array is not empty and not all empty
func ValidateMessages(messages []models.Message) error {
	if len(messages) == 0 {
		return fmt.Errorf("messages array cannot be empty")
	}

	allEmpty := true
	for _, msg := range messages {
		if strings.TrimSpace(msg.Content) != "" {
			allEmpty = false
			break
		}
	}

	if allEmpty {
		return fmt.Errorf("all messages have empty content")
	}

	return nil
}

// ValidateGenerationRequest validates common generation request parameters
func ValidateGenerationRequest(model string, maxTokens int, temperature float32) error {
	if maxTokens < 0 {
		return fmt.Errorf("max_tokens must be non-negative")
	}

	if temperature < 0 || temperature > 2 {
		return fmt.Errorf("temperature must be between 0 and 2")
	}

	return nil
}

// MarshalJSONSafely marshals JSON and logs errors instead of silently failing
func MarshalJSONSafely(log *zap.Logger, v interface{}) []byte {
	data, err := json.Marshal(v)
	if err != nil {
		log.Error("Failed to marshal JSON", zap.Error(err), zap.Any("value", v))
		return []byte("{}")
	}
	return data
}

// SendStreamChunk writes a JSON chunk to the stream writer with error handling
func SendStreamChunk(w *bufio.Writer, log *zap.Logger, chunk interface{}) error {
	data := MarshalJSONSafely(log, chunk)
	if _, err := w.Write(data); err != nil {
		log.Error("Failed to write chunk", zap.Error(err))
		return err
	}
	if _, err := w.Write([]byte("\n")); err != nil {
		log.Error("Failed to write newline", zap.Error(err))
		return err
	}
	if err := w.Flush(); err != nil {
		log.Error("Failed to flush writer", zap.Error(err))
		return err
	}
	return nil
}

// SendSSEChunk writes a Server-Sent Event chunk
func SendSSEChunk(w *bufio.Writer, log *zap.Logger, event string, chunk interface{}) error {
	data := MarshalJSONSafely(log, chunk)
	if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, string(data)); err != nil {
		log.Error("Failed to write SSE chunk", zap.Error(err))
		return err
	}
	if err := w.Flush(); err != nil {
		log.Error("Failed to flush SSE writer", zap.Error(err))
		return err
	}
	return nil
}

// SendSSEEvent writes a generic SSE data event by marshaling v as JSON.
// It returns false if writing fails (to signal the caller to stop streaming).
func SendSSEEvent(w *bufio.Writer, log *zap.Logger, v interface{}) bool {
	data := MarshalJSONSafely(log, v)
	if _, err := fmt.Fprintf(w, "data: %s\n\n", string(data)); err != nil {
		log.Error("Failed to write SSE event", zap.Error(err))
		return false
	}
	if err := w.Flush(); err != nil {
		log.Error("Failed to flush SSE event writer", zap.Error(err))
		return false
	}
	return true
}


// SplitResponseIntoChunks simulates streaming by splitting response into chunks
func SplitResponseIntoChunks(text string, delayMs int) []string {
	words := strings.Split(text, " ")
	var chunks []string
	for i, word := range words {
		content := word
		if i < len(words)-1 {
			content += " "
		}
		chunks = append(chunks, content)
	}
	return chunks
}

// SleepWithCancel sleeps for the specified duration or until context is cancelled
func SleepWithCancel(ctx context.Context, duration time.Duration) bool {
	select {
	case <-time.After(duration):
		return true
	case <-ctx.Done():
		return false
	}
}

// ErrorToResponse converts an error to a standardized error response
func ErrorToResponse(err error, errorType string) models.ErrorResponse {
	return models.ErrorResponse{
		Error: models.Error{
			Message: err.Error(),
			Type:    errorType,
		},
	}
}

// CleanJSONBlock extracts valid JSON from Markdown blocks (e.g., ```json ... ```).
// It safely handles conversational filler before or after the JSON.
func CleanJSONBlock(text string) string {
	// 1. Look for explicitly marked JSON code blocks
	startIdx := strings.Index(text, "```json")
	if startIdx != -1 {
		codeStart := startIdx + len("```json")
		endIdx := strings.LastIndex(text, "```")
		if endIdx != -1 && endIdx > codeStart {
			candidate := strings.TrimSpace(text[codeStart:endIdx])
			// Fast-track if valid
			var m interface{}
			if json.Unmarshal([]byte(candidate), &m) == nil {
				return candidate
			}
			// Attempt to recover if malformed (brute force closing braces)
			if recovered := recoverJSON(candidate); recovered != "" {
				return recovered
			}
			return candidate
		}
	}

	trimmed := strings.TrimSpace(text)
	
	// 2. Fallback: Extract aggressive JSON block if it prominently contains "tool_calls" or looks like an object
	if strings.Contains(trimmed, "\"tool_calls\"") || strings.Contains(trimmed, "\"function\"") || strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
		searchStart := 0
		if tcIdx := strings.Index(trimmed, "\"tool_calls\""); tcIdx != -1 {
			// Find the opening brace BEFORE "tool_calls"
			if lastOpen := strings.LastIndex(trimmed[:tcIdx], "{"); lastOpen != -1 {
				searchStart = lastOpen
			}
		} else if fnIdx := strings.Index(trimmed, "\"function\""); fnIdx != -1 {
			// Find the opening brace BEFORE "function"
			if lastOpen := strings.LastIndex(trimmed[:fnIdx], "{"); lastOpen != -1 {
				searchStart = lastOpen
			}
		} else {
			// Just find the first brace or bracket
			firstBrace := strings.Index(trimmed, "{")
			firstBracket := strings.Index(trimmed, "[")
			if firstBrace != -1 && (firstBracket == -1 || firstBrace < firstBracket) {
				searchStart = firstBrace
			} else if firstBracket != -1 {
				searchStart = firstBracket
			}
		}

		if recovered := recoverJSON(trimmed[searchStart:]); recovered != "" {
			return recovered
		}
		
		// Simple fallback to last brace/bracket if recovery failed
		lastBrace := strings.LastIndex(trimmed, "}")
		lastBracket := strings.LastIndex(trimmed, "]")
		endIdx := -1
		if lastBrace != -1 && (lastBracket == -1 || lastBrace > lastBracket) {
			endIdx = lastBrace
		} else if lastBracket != -1 {
			endIdx = lastBracket
		}

		if endIdx > searchStart {
			return strings.TrimSpace(trimmed[searchStart : endIdx+1])
		}
	}

	// 3. Fallback: Clean up plain backticks if the text starts with them
	if strings.HasPrefix(trimmed, "```") {
		trimmed = strings.TrimPrefix(trimmed, "```")
		trimmed = strings.TrimSuffix(trimmed, "```")
		trimmed = strings.TrimSpace(trimmed)
	}

	return trimmed
}

// recoverJSON attempts to find the largest valid JSON object by trying different closing positions.
// This is crucial for handling Gemini's occasional extra braces like }}}} at the end.
func recoverJSON(s string) string {
	// Try finding largest valid JSON object {}
	for last := strings.LastIndex(s, "}"); last != -1; last = strings.LastIndex(s[:last], "}") {
		candidate := s[:last+1]
		var m interface{}
		if json.Unmarshal([]byte(candidate), &m) == nil {
			return candidate
		}
	}
	// Try finding largest valid JSON array []
	for last := strings.LastIndex(s, "]"); last != -1; last = strings.LastIndex(s[:last], "]") {
		candidate := s[:last+1]
		var m interface{}
		if json.Unmarshal([]byte(candidate), &m) == nil {
			return candidate
		}
	}
	return ""
}

// NormalizeJSON cleans up AI-over-escaped characters like \_ that break JSON parsing.
func NormalizeJSON(jsonStr string) string {
	// Remove common over-escaped underscores
	return strings.ReplaceAll(jsonStr, "\\_", "_")
}

// TruncateString truncates a string to the specified length and adds ... if it exceeds it.
func TruncateString(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

// StripMarkdownLink extracts the URL from a markdown link format like [label](url).
// It also handles common Gemini patterns like "label (url)" or "(url)" trailing text.
func StripMarkdownLink(text string) string {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return text
	}

	// 1. Handle [label](url)
	if strings.HasPrefix(trimmed, "[") {
		bracketEnd := strings.Index(trimmed, "](")
		if bracketEnd != -1 {
			parenEnd := strings.LastIndex(trimmed, ")")
			if parenEnd != -1 && parenEnd > bracketEnd+2 {
				url := trimmed[bracketEnd+2 : parenEnd]
				return strings.TrimSpace(url)
			}
		}
	}

	// 2. Handle redundant URL in parenthesis at the end of a string: "some command (https://url.com)"
	// This frequently breaks shell commands if left in.
	if strings.HasSuffix(trimmed, ")") {
		lastLParen := strings.LastIndex(trimmed, "(")
		if lastLParen != -1 && lastLParen < len(trimmed)-1 {
			content := trimmed[lastLParen+1 : len(trimmed)-1]
			content = strings.TrimSpace(content)
			
			// Detect if the parenthetical content is an absolute URL
			if strings.HasPrefix(content, "http://") || strings.HasPrefix(content, "https://") {
				// Strip the URL and the parenthesis, along with any preceding space
				prefix := strings.TrimSpace(trimmed[:lastLParen])
				if prefix != "" {
					return prefix
				}
				return content // return just the URL if there's no prefix
			}
		}
	}

	return text
}

// ExtractContextText removes the code blocks or raw JSON payload from the text to preserve the surrounding narrative context.
func ExtractContextText(text, jsonPayload string) string {
	cleaned := text

	// 1. Remove all ```json ... ``` blocks
	for {
		startIdx := strings.Index(cleaned, "```json")
		if startIdx == -1 {
			break
		}
		
		endIdx := strings.Index(cleaned[startIdx+7:], "```")
		if endIdx == -1 {
			// Unclosed block, just remove the marker and continue
			cleaned = cleaned[:startIdx] + cleaned[startIdx+7:]
			continue
		}
		
		fullBlockEnd := startIdx + 7 + endIdx + 3
		before := strings.TrimSpace(cleaned[:startIdx])
		after := strings.TrimSpace(cleaned[fullBlockEnd:])
		
		if before != "" && after != "" {
			cleaned = before + "\n\n" + after
		} else {
			cleaned = before + after
		}
	}

	// 2. If jsonPayload is provided and it exists in the raw text, remove it!
	// This handles cases where Gemini omits the ```json markdown wrapper.
	if jsonPayload != "" && strings.Contains(cleaned, jsonPayload) {
		// Only remove if it's large enough to be a likely JSON block, 
		// or if it's the dominant part of the text.
		cleaned = strings.Replace(cleaned, jsonPayload, "", 1)
		cleaned = strings.TrimSpace(cleaned)
	}

	return cleaned
}
