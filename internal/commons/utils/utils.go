package utils

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"gemini-web-to-api/internal/commons/models"

	"go.uber.org/zap"
)

// JSONCandidate holds an extracted JSON block and its metadata
type JSONCandidate struct {
	RawPayload string
	StartIndex int
	EndIndex   int
}

// BuildPromptFromMessages constructs a unified prompt from messages
func BuildPromptFromMessages(messages []models.Message, systemPrompt string) string {
	var promptBuilder strings.Builder

	if systemPrompt != "" {
		promptBuilder.WriteString(fmt.Sprintf("%s\n\n", systemPrompt))
	}

	for _, msg := range messages {
		if strings.EqualFold(msg.Role, "system") {
			promptBuilder.WriteString(fmt.Sprintf("%s\n\n", msg.Content))
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

// RawWrite writes raw bytes to the stream writer and flushes it
func RawWrite(w *bufio.Writer, log *zap.Logger, data []byte) error {
	if _, err := w.Write(data); err != nil {
		log.Error("Failed to write raw bytes", zap.Error(err))
		return err
	}
	return w.Flush()
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


// SplitStringChunks splits a string into chunks of approximately chunkSize.
// It uses rune slices to ensure multi-byte characters are not split in half.
func SplitStringChunks(s string, chunkSize int) []string {
	if chunkSize <= 0 {
		return []string{s}
	}
	
	runes := []rune(s)
	total := len(runes)
	if total <= chunkSize {
		return []string{s}
	}
	
	var chunks []string
	for i := 0; i < total; i += chunkSize {
		end := i + chunkSize
		if end > total {
			end = total
		}
		chunks = append(chunks, string(runes[i:end]))
	}
	return chunks
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

// CleanJSONBlock extracts the single best valid JSON block from text.
func CleanJSONBlock(text string) string {
	blocks, _ := ExtractAllJSONBlocks(text)
	if len(blocks) > 0 {
		return blocks[0]
	}

	trimmed := strings.TrimSpace(text)
	if strings.Contains(trimmed, "\"tool_calls\"") || strings.Contains(trimmed, "\"function\"") || strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
		if recovered := recoverJSON(trimmed); recovered != "" {
			return recovered
		}
	}
	
	if strings.HasPrefix(trimmed, "```") {
		trimmed = strings.TrimPrefix(trimmed, "```")
		trimmed = strings.TrimSuffix(trimmed, "```")
		trimmed = strings.TrimSpace(trimmed)
	}

	return trimmed
}

var (
	// jsonBlockRegex matches anything that looks like a JSON block containing tool call keywords.
	// V18: Support both {} and [] blocks, and add "call_" as a keyword.
	jsonBlockRegex = regexp.MustCompile(`(?s)(?:\{|\[).*?"(?:tool_calls|function|function_call|call_)".*?(?:\}|\])`)
	// Heuristic regexes for reassembly
	hNameRegex    = regexp.MustCompile(`(?i)"name"\s*:\s*"([^"]+)"`)
	hIdRegex      = regexp.MustCompile(`(?i)"id"\s*:\s*"([^"]+)"`)
	hPathRegex    = regexp.MustCompile(`(?i)"path"\s*:\s*"([^"]+)"`)
	hCommandRegex = regexp.MustCompile(`(?i)"command"\s*:\s*"([^"]+)"`)
	hTodoRegex    = regexp.MustCompile(`(?s)\{\s*"status"\s*:\s*"([^"]+)"\s*,\s*"task"\s*:\s*"([^"]+)"\s*\}`)
	// Content is greedier: find everything between "content": " and the next key or the end of the block
	hContentRegex = regexp.MustCompile(`(?s)"content"\s*:\s*"(.*)"`)
)

// ExtractAllJSONBlocks finds all JSON blocks and returns (parsedBlocks, sourceSegments).
func ExtractAllJSONBlocks(text string) ([]string, []string) {
	var parsedBlocks []string
	var sourceSegments []string

	// 1. First, extract all markdown blocks ```json ... ``` (Highest priority)
	remaining := text
	for {
		startIdx := strings.Index(remaining, "```json")
		if startIdx == -1 {
			break
		}
		markerLen := len("```json")
		contentStart := startIdx + markerLen
		endIdx := strings.Index(remaining[contentStart:], "```")
		if endIdx == -1 {
			candidate := strings.TrimSpace(remaining[contentStart:])
			fullSource := remaining[startIdx:]
			if recovered := recoverJSON(candidate); recovered != "" {
				parsedBlocks = append(parsedBlocks, recovered)
				sourceSegments = append(sourceSegments, fullSource)
			}
			break
		}
		fullEndIdx := contentStart + endIdx
		candidate := strings.TrimSpace(remaining[contentStart:fullEndIdx])
		parsedBlocks = append(parsedBlocks, candidate)
		sourceSegments = append(sourceSegments, remaining[startIdx:fullEndIdx+3])
		remaining = remaining[fullEndIdx+3:]
	}

	// 2. Use "Nuclear" Regex extraction for raw JSON if no markdown found
	if len(parsedBlocks) == 0 {
		normalizedText := NormalizeJSON(text)
		matches := jsonBlockRegex.FindAllStringIndex(normalizedText, -1)
		for _, match := range matches {
			rawCandidate := text[match[0]:match[1]]
			if recovered := recoverJSON(rawCandidate); recovered != "" {
				parsedBlocks = append(parsedBlocks, recovered)
				sourceSegments = append(sourceSegments, rawCandidate)
			}
		}
	}

	// 3. HEURISTIC REASSEMBLY (V9): Search for Skeleton Actions
	toolNames := []string{"write_to_file", "update_todo_list", "execute_command", "replace_file_content"}
	normalizedText := NormalizeJSON(text)
	
	for _, toolName := range toolNames {
		if strings.Contains(normalizedText, toolName) {
			nameIndices := regexp.MustCompile(fmt.Sprintf(`(?i)%s`, toolName)).FindAllStringIndex(normalizedText, -1)
			for _, idx := range nameIndices {
				start := idx[0] - 500
				if start < 0 { start = 0 }
				end := idx[1] + 3000
				if end > len(normalizedText) { end = len(normalizedText) }
				window := normalizedText[start:end]
				rawWindow := text[start:end]
				
				matchId := hIdRegex.FindStringSubmatch(window)
				matchPath := hPathRegex.FindStringSubmatch(window)
				matchCommand := hCommandRegex.FindStringSubmatch(window)
				id := "call_" + toolName
				if len(matchId) > 1 { id = matchId[1] }
				
				content := ""
				if toolName == "write_to_file" || toolName == "replace_file_content" {
					contentStartIdx := strings.Index(window, "\"content\": \"")
					if contentStartIdx != -1 {
						valStart := contentStartIdx + len("\"content\": \"")
						bestDecoded := ""
						rawValInWindow := window[valStart:]
						for i := 0; i < len(rawValInWindow); i++ {
							if rawValInWindow[i] == '"' {
								candidate := rawValInWindow[:i]
								var decoded string
								if err := json.Unmarshal([]byte("\"" + candidate + "\""), &decoded); err == nil {
									if len(decoded) > len(bestDecoded) {
										bestDecoded = decoded
									}
								}
							}
						}
						content = bestDecoded
					}
				}

				var params string
				if toolName == "write_to_file" && len(matchPath) > 1 {
					pathJson, _ := json.Marshal(matchPath[1])
					// V11: Strip Markdown Links from content
					cleanContent := StripMarkdownLink(content)
					contentJson, _ := json.Marshal(cleanContent)
					params = fmt.Sprintf(`{"path": %s, "content": %s}`, string(pathJson), string(contentJson))
				} else if toolName == "execute_command" && len(matchCommand) > 1 {
					// V11: Strip Markdown Links from command
					cleanCmd := StripMarkdownLink(matchCommand[1])
					cmdJson, _ := json.Marshal(cleanCmd)
					params = fmt.Sprintf(`{"command": %s}`, string(cmdJson))
				} else if toolName == "update_todo_list" {
					todoMatches := hTodoRegex.FindAllStringSubmatch(window, -1)
					if len(todoMatches) > 0 {
						var items []string
						for _, m := range todoMatches {
							sJ, _ := json.Marshal(m[1]); tJ, _ := json.Marshal(m[2])
							items = append(items, fmt.Sprintf(`{"status": %s, "task": %s}`, string(sJ), string(tJ)))
						}
						params = fmt.Sprintf(`{"todos": [%s]}`, strings.Join(items, ","))
					}
				}
				
				if params != "" {
					cId, _ := json.Marshal(id); cName, _ := json.Marshal(toolName)
					reassembled := fmt.Sprintf(`{"id": %s, "type": "function", "function": {"name": %s, "parameters": %s}}`, string(cId), string(cName), params)
					
					alreadyCovered := false
					for _, s := range sourceSegments {
						if strings.Contains(s, rawWindow) || strings.Contains(rawWindow, s) {
							alreadyCovered = true
							break
						}
					}
					if !alreadyCovered {
						parsedBlocks = append(parsedBlocks, reassembled)
						sourceSegments = append(sourceSegments, rawWindow)
					}
				}
			}
		}
	}

	// 4. Last fallback
	if len(parsedBlocks) == 0 {
		trimmed := strings.TrimSpace(text)
		if strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
			if recovered := recoverJSON(trimmed); recovered != "" {
				parsedBlocks = append(parsedBlocks, recovered)
				sourceSegments = append(sourceSegments, trimmed)
			}
		}
	}

	return parsedBlocks, sourceSegments
}

// recoverJSON attempts to find the largest valid JSON object by trying different closing positions.
// It uses tentative normalization to detect "broken" JSON blocks.
func recoverJSON(s string) string {
	// Try finding largest valid JSON object {}
	for last := strings.LastIndex(s, "}"); last != -1; last = strings.LastIndex(s[:last], "}") {
		candidate := s[:last+1]
		var m interface{}
		// Use tentative normalization for validation
		normalized := NormalizeJSON(candidate)
		if json.Unmarshal([]byte(normalized), &m) == nil {
			return candidate
		}
	}
	// Try finding largest valid JSON array []
	for last := strings.LastIndex(s, "]"); last != -1; last = strings.LastIndex(s[:last], "]") {
		candidate := s[:last+1]
		var m interface{}
		normalized := NormalizeJSON(candidate)
		if json.Unmarshal([]byte(normalized), &m) == nil {
			return candidate
		}
	}
	return ""
}

var (
	// unescapeRegex matches a backslash followed by any character.
	unescapeRegex = regexp.MustCompile(`\\(.)`)
	// legalJSONEscapes contains characters that are ALREADY part of standard JSON escapes.
	// We MUST NOT strip the backslash before these.
	legalJSONEscapes = "nrbtfu\\/\""
)

// NormalizeJSON cleans up AI-over-escaped characters that break JSON parsing.
func NormalizeJSON(jsonStr string) string {
	if jsonStr == "" {
		return jsonStr
	}

	// Gemini Web frequently "escapes" characters for Markdown safety (e.g., \_, \\_).
	
	// 1. First, handle the most common double-escaped keys like \\_
	// This appears frequently in Gemini Web's "raw" output.
	jsonStr = strings.ReplaceAll(jsonStr, "\\\\_", "_")

	// 2. Use Regex for any other single-escaped illegal characters
	result := unescapeRegex.ReplaceAllStringFunc(jsonStr, func(match string) string {
		if len(match) < 2 {
			return match
		}
		char := match[1:2]
		// If the second character is a standard JSON escape char, keep the backslash
		// Legal escapes according to RFC 8259 are: " \ / b f n r t uXXXX
		if strings.ContainsAny(char, legalJSONEscapes) {
			return match
		}
		// Otherwise, it's an illegal escape for JSON (like \_ or \. or \<), strip it
		return char
	})

	return result
}

// TruncateString truncates a string to the specified length and adds ... if it exceeds it.
func TruncateString(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

var mdLinkRegex = regexp.MustCompile(`\[([^\]]+)\]\((https?://[^\)]+)\)`)

// StripMarkdownLink removes markdown-style links [Label](URL) and returns only the Label.
func StripMarkdownLink(text string) string {
	if text == "" {
		return text
	}
	// 1. Replace all global [Label](URL) with just Label
	// Gemini Web frequently wraps domains/packages in links.
	cleaned := mdLinkRegex.ReplaceAllString(text, "$1")

	// 2. Extra cleaning: remove bracketed URLs if trailing: "command (http://...)"
	parenUrlRegex := regexp.MustCompile(`\s*\(\s*https?://[^\s\)]+\s*\)`)
	cleaned = parenUrlRegex.ReplaceAllString(cleaned, "")

	return strings.TrimSpace(cleaned)
}

// ExtractContextText removes the code blocks or raw JSON payload from the text to preserve the surrounding narrative context.
func ExtractContextText(text string, sourceSegments []string) string {
	cleaned := text

	// 1. Remove all successfully identified original segments (including heuristic windows)
	for _, segment := range sourceSegments {
		if segment != "" && strings.Contains(cleaned, segment) {
			cleaned = strings.Replace(cleaned, segment, "", 1)
		}
	}

	// 2. Extra cleaning: remove markdown markers if left behind
	cleaned = strings.ReplaceAll(cleaned, "```json", "")
	cleaned = strings.ReplaceAll(cleaned, "```", "")

	return strings.TrimSpace(cleaned)
}

// HasMarkdownLink checks if the string contains any Markdown-formatted links [label](url)
func HasMarkdownLink(s string) bool {
	re := regexp.MustCompile(`\[.*?\]\(.*?\)`)
	return re.MatchString(s)
}

// ContainsJunkIndicators checks if the text contains Gemini Web frontend "junk" 
// that should not be returned to an API client (e.g. interactive chips, canvas UI markers).
func ContainsJunkIndicators(text string) bool {
	junkMarkers := []string{
		"immersive_entry_chips",
		"chip_cloud",
		"render_immersive",
		"action_chips",
		"immersive-view",
		"button_chip",
	}
	
	lower := strings.ToLower(text)
	for _, marker := range junkMarkers {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}
