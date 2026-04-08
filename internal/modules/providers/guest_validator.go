package providers

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/imroc/req/v3"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"
)

// GuestValidateResult contains the result of a raw-call validation for a guest platform
type GuestValidateResult struct {
	Platform   string `json:"platform"`
	BaseURL    string `json:"base_url"`
	IsValid    bool   `json:"is_valid"`
	StatusCode int    `json:"status_code"`
	TextFound  string `json:"text_found"`
	Error      string `json:"error,omitempty"`
	Method     string `json:"method"` // "gemini-guest" | "raw-payload" | "skipped"
}

// ValidateAllGuestsByRawCall validates each invalid platform config by:
//  1. If platform == "gemini": use Gemini guest mode (no cookie)
//  2. Otherwise: POST the stored PayloadTemplate directly to the platform's captured API endpoint
//
// On success → updates is_valid=true in MultiGuestManager.
// On failure → logs detailed error, leaves is_valid=false.
func (c *Client) ValidateAllGuestsByRawCall(ctx context.Context) []GuestValidateResult {
	configs := c.multiGuestMgr.GetConfigs()
	results := make([]GuestValidateResult, 0, len(configs))

	for _, pc := range configs {
		result := c.ValidateSingleGuestByRawCall(ctx, pc.Name)
		results = append(results, result)
	}

	return results
}

// ValidateSingleGuestByRawCall validates a specific platform config.
func (c *Client) ValidateSingleGuestByRawCall(ctx context.Context, platformName string) GuestValidateResult {
	pc, ok := c.multiGuestMgr.GetConfig(platformName)
	if !ok {
		return GuestValidateResult{
			Platform: platformName,
			IsValid:  false,
			Error:    "platform config not found",
		}
	}

	if pc.Disabled {
		c.log.Info("⏭️  Validator: skipping DISABLED platform", zap.String("platform", pc.Name))
		return GuestValidateResult{
			Platform: pc.Name,
			BaseURL:  pc.BaseURL,
			IsValid:  false,
			Method:   "skipped-disabled",
		}
	}

	c.log.Info("🔬 Validator: Testing platform via raw call",
		zap.String("platform", pc.Name),
		zap.String("base_url", pc.BaseURL),
		zap.Bool("currently_valid", pc.IsValid),
		zap.String("candidate_path", pc.GJSONPaths.CandidatePath),
		zap.String("text_path", pc.GJSONPaths.TextPath),
	)

	pCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	var result GuestValidateResult
	if pc.Name == "gemini" {
		result = c.validateGeminiGuest(pCtx, pc)
	} else {
		result = c.validateByRawPayload(pCtx, pc)
	}

	if result.IsValid {
		c.log.Info("✅ Validator: VALID — updating is_valid=true",
			zap.String("platform", pc.Name),
			zap.String("text_sample", truncateStr(result.TextFound, 80)),
		)
		updated := pc
		updated.IsValid = true
		updated.FailCount = 0
		c.multiGuestMgr.SaveConfig(updated)

		// Ensure worker exists in pool
		c.mu.Lock()
		if _, ok := c.guestWorkers[pc.Name]; !ok {
			gw := NewGuestWorker(c.cfg, c.log, c)
			gw.PlatformName = pc.Name
			c.guestWorkers[pc.Name] = gw
		}
		c.mu.Unlock()
	} else {
		c.log.Warn("❌ Validator: INVALID",
			zap.String("platform", pc.Name),
			zap.Int("status_code", result.StatusCode),
			zap.String("error", result.Error),
		)
	}

	return result
}

// validateGeminiGuest tests "gemini" platform using standard Gemini guest API (no cookies)
func (c *Client) validateGeminiGuest(ctx context.Context, pc PlatformConfig) GuestValidateResult {
	result := GuestValidateResult{
		Platform: pc.Name,
		BaseURL:  pc.BaseURL,
		Method:   "gemini-guest",
	}

	// Use the existing GuestWorker if available
	gw, ok := c.GetGuestWorker("gemini")
	if !ok {
		gw = NewGuestWorker(c.cfg, c.log, c)
		gw.PlatformName = "gemini"
	}

	// Temporarily mark as valid to bypass the is_valid guard
	savedConfig := pc
	savedConfig.IsValid = true
	c.multiGuestMgr.SaveConfig(savedConfig)

	resp, err := gw.GenerateContent(ctx, "Respond ONLY with 'ok' and nothing else. No greeting, no markdown.")

	// Restore original validity state if it fails
	if err != nil {
		savedConfig.IsValid = false
		c.multiGuestMgr.SaveConfig(savedConfig)
		result.Error = fmt.Sprintf("gemini guest call failed: %v", err)
		return result
	}

	if resp == nil || resp.Text == "" {
		savedConfig.IsValid = false
		c.multiGuestMgr.SaveConfig(savedConfig)
		result.Error = "gemini guest returned empty response"
		return result
	}

	result.IsValid = true
	result.TextFound = resp.Text
	return result
}

// validateByRawPayload tests a non-Gemini platform by POSTing its captured PayloadTemplate
// directly to its captured API endpoint (extracted from the payload_template metadata).
func (c *Client) validateByRawPayload(ctx context.Context, pc PlatformConfig) GuestValidateResult {
	result := GuestValidateResult{
		Platform: pc.Name,
		BaseURL:  pc.BaseURL,
		Method:   "raw-payload",
	}

	if pc.PayloadTemplate == "" {
		result.Error = "no payload_template stored — cannot validate without browser re-learn"
		return result
	}

	// Detect the API endpoint from the payload structure
	apiEndpoint := detectAPIEndpoint(pc)
	if apiEndpoint == "" {
		result.Error = fmt.Sprintf("cannot determine API endpoint for %s (base_url=%s), needs browser re-learn", pc.Name, pc.BaseURL)
		c.log.Warn("🔍 Validator: Cannot determine API endpoint, payload structure unknown",
			zap.String("platform", pc.Name),
			zap.String("base_url", pc.BaseURL),
			zap.String("payload_prefix", truncateStr(pc.PayloadTemplate, 200)),
		)
		return result
	}

	c.log.Info("🌐 Validator: Sending raw payload to detected endpoint",
		zap.String("platform", pc.Name),
		zap.String("endpoint", apiEndpoint),
		zap.String("payload_size", fmt.Sprintf("%d bytes", len(pc.PayloadTemplate))),
	)

	httpClient := req.NewClient().
		ImpersonateChrome().
		SetTimeout(30 * time.Second).
		SetCommonHeaders(map[string]string{
			"Content-Type": "application/json",
			"Accept":       "application/json, text/event-stream, */*",
			"Referer":      pc.BaseURL,
			"Origin":       extractOrigin(pc.BaseURL),
		})

	resp, err := httpClient.R().
		SetContext(ctx).
		SetBodyString(pc.PayloadTemplate).
		Post(apiEndpoint)

	if err != nil {
		result.Error = fmt.Sprintf("HTTP request failed: %v", err)
		return result
	}

	result.StatusCode = resp.StatusCode

	c.log.Info("📨 Validator: Got response",
		zap.String("platform", pc.Name),
		zap.Int("status", resp.StatusCode),
		zap.Int("response_bytes", len(resp.String())),
	)

	if resp.StatusCode != http.StatusOK {
		body := resp.String()
		result.Error = fmt.Sprintf("HTTP %d: %s", resp.StatusCode, truncateStr(body, 300))
		c.log.Warn("❌ Validator: Non-200 response",
			zap.String("platform", pc.Name),
			zap.Int("status", resp.StatusCode),
			zap.String("body_sample", truncateStr(body, 500)),
		)
		return result
	}

	// Try to extract text using the stored gjson_paths
	body := resp.String()
	c.log.Info("🔍 Validator: Parsing response with stored gjson_paths",
		zap.String("platform", pc.Name),
		zap.String("candidate_path", pc.GJSONPaths.CandidatePath),
		zap.String("text_path", pc.GJSONPaths.TextPath),
		zap.String("body_sample", truncateStr(body, 500)),
	)

	extractedText := extractTextFromResponse(body, pc.GJSONPaths)

	if extractedText == "" {
		// Try Gemini streaming format (multi-line)
		extractedText = extractTextFromStreamingResponse(body, pc.GJSONPaths)
	}

	if extractedText != "" {
		result.IsValid = true
		result.TextFound = extractedText
	} else {
		result.Error = fmt.Sprintf(
			"response OK but gjson_paths %q→%q found no text. Raw body sample: %s",
			pc.GJSONPaths.CandidatePath, pc.GJSONPaths.TextPath,
			truncateStr(body, 400),
		)
		c.log.Warn("🔍 Validator: GJSON extraction returned empty — paths may be wrong",
			zap.String("platform", pc.Name),
			zap.String("candidate_path", pc.GJSONPaths.CandidatePath),
			zap.String("text_path", pc.GJSONPaths.TextPath),
			zap.String("full_body", truncateStr(body, 2000)),
		)
	}

	return result
}

// detectAPIEndpoint tries to infer the actual API endpoint from the platform's base_url and payload.
// In the discovery phase, the browser captures the network request body but we don't store
// the exact endpoint URL — only the base_url. This function tries common patterns.
func detectAPIEndpoint(pc PlatformConfig) string {
	base := strings.TrimRight(pc.BaseURL, "/")
	payload := pc.PayloadTemplate

	// Known patterns per platform characteristics
	switch {
	case strings.Contains(pc.BaseURL, "notegpt.io"):
		return "https://notegpt.io/api/v1/ai/chat/stream"
	case strings.Contains(pc.BaseURL, "chatbotchatapp.com"):
		return "https://chatbotchatapp.com/api/chat"
	case strings.Contains(pc.BaseURL, "deepai.org"):
		return "https://deepai.org/api/v2/chat"
	case strings.Contains(pc.BaseURL, "easemate.ai"):
		return "https://www.easemate.ai/api/ai/get_assistant_response_stream"
	case strings.Contains(pc.BaseURL, "perplexity.ai"):
		return "https://www.perplexity.ai/rest/sse/perplexity_ask"
	case strings.Contains(pc.BaseURL, "ai.updf.com"):
		return "https://ai.updf.com/api/v1/assistant/gemini/stream"
	case strings.Contains(pc.BaseURL, "chatgpt.com"):
		// ChatGPT requires auth tokens - won't work as guest
		return ""
	case strings.Contains(pc.BaseURL, "copilot.microsoft.com"):
		// Copilot uses websockets/SSE - not simple POST
		return ""
	case strings.Contains(base, "gemini.google.com"):
		// These should be validated via gemini guest mode
		return EndpointGenerate
	}

	// Check payload to detect endpoint hints
	if strings.Contains(payload, "conversation_id") && strings.Contains(payload, "message") {
		return base + "/api/chat"
	}
	if strings.Contains(payload, "messages") && strings.Contains(payload, "role") {
		return base + "/api/v1/chat/completions"
	}

	return ""
}

// extractTextFromResponse tries to extract text from a JSON response using gjson paths
func extractTextFromResponse(body string, schema ExtractorSchema) string {
	if !gjson.Valid(body) {
		return ""
	}

	candidatesResult := gjson.Get(body, schema.CandidatePath)
	if !candidatesResult.Exists() {
		return ""
	}

	if candidatesResult.IsArray() {
		arr := candidatesResult.Array()
		if len(arr) == 0 {
			return ""
		}
		text := gjson.Get(arr[0].Raw, schema.TextPath).String()
		return text
	}

	// It's a scalar, try text_path from full body
	return gjson.Get(body, schema.CandidatePath+"."+schema.TextPath).String()
}

// extractTextFromStreamingResponse handles Gemini-style chunked streaming responses
func extractTextFromStreamingResponse(body string, schema ExtractorSchema) string {
	lines := strings.Split(body, "\n")
	var bestText string

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if len(line) < 5 || !gjson.Valid(line) {
			continue
		}

		candidatesResult := gjson.Get(line, schema.CandidatePath)
		if !candidatesResult.IsArray() || len(candidatesResult.Array()) == 0 {
			continue
		}

		firstCandidate := candidatesResult.Array()[0]
		text := gjson.Get(firstCandidate.Raw, schema.TextPath).String()
		if text != "" && len(text) > len(bestText) {
			bestText = text
		}
	}

	return bestText
}

// extractOrigin extracts the origin from a URL (scheme + host)
func extractOrigin(rawURL string) string {
	if idx := strings.Index(rawURL, "//"); idx != -1 {
		rest := rawURL[idx+2:]
		if slashIdx := strings.Index(rest, "/"); slashIdx != -1 {
			return rawURL[:idx+2+slashIdx]
		}
		return rawURL
	}
	return rawURL
}

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
