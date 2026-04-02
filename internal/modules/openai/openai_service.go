package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"gemini-web-to-api/internal/commons/configs"
	"gemini-web-to-api/internal/commons/models"
	"gemini-web-to-api/internal/commons/utils"
	"gemini-web-to-api/internal/modules/openai/dto"
	"gemini-web-to-api/internal/modules/providers"
	"errors"

	"go.uber.org/zap"
)

type OpenAIService struct {
	client *providers.Client
	log    *zap.Logger
	cfg    *configs.Config
}

func NewOpenAIService(client *providers.Client, log *zap.Logger, cfg *configs.Config) *OpenAIService {
	return &OpenAIService{
		client: client,
		log:    log,
		cfg:    cfg,
	}
}

func (s *OpenAIService) ListModels() []providers.ModelInfo {
	return s.client.ListModels()
}

func (s *OpenAIService) CreateChatCompletion(ctx context.Context, req dto.ChatCompletionRequest) (*dto.ChatCompletionResponse, error) {
	s.log.Info("==================================================")
	s.log.Info("📥 1. NEW REQUEST: OpenAI ChatCompletion")
	if s.cfg.Gemini.LogRawRequests {
		reqBytes, _ := json.MarshalIndent(req, "", "  ")
		s.log.Info(fmt.Sprintf("Request Payload:\n%s", string(reqBytes)))
	}

	// Logic: Validate messages
	if err := utils.ValidateMessages(req.Messages); err != nil {
		return nil, err
	}

	// Logic: Validate generation parameters
	if err := utils.ValidateGenerationRequest(req.Model, req.MaxTokens, req.Temperature); err != nil {
		return nil, err
	}

	// Always stateless prompt execution as requested by the user
	prompt := utils.BuildPromptFromMessages(req.Messages, "")
	if prompt == "" {
		return nil, fmt.Errorf("no valid content in messages")
	}
	
	// Inject tool instruction if the client registered tools OR if it expects structured parsing.
	if len(req.Tools) > 0 {
		toolPrompt := "\n\n# INSTRUCTION: TOOL EXECUTION FORMAT\n" +
			"The user has provided functions for you to use. If you need to perform an action, you MUST respond with a valid JSON object containing `tool_calls`. \n" +
			"Rules for JSON format:\n" +
			"1. Use EXACTLY the following structure:\n" +
			"```json\n" +
			"{\n" +
			"  \"tool_calls\": [\n" +
			"    {\n" +
			"      \"id\": \"call_unique_hex_id\",\n" +
			"      \"type\": \"function\",\n" +
			"      \"function\": {\n" +
			"        \"name\": \"function_name\",\n" +
			"        \"parameters\": {\n" +
			"          \"key\": \"value\"\n" +
			"        }\n" +
			"      }\n" +
			"    }\n" +
			"  ]\n" +
			"}\n" +
			"```\n" +
			"2. IMPORTANT: Do NOT escape underscores in JSON identifiers. Return \"tool_calls\", NOT \"tool\\_calls\".\n" +
			"3. CRITICAL: NEVER use hidden markdown links like `[url](url)` or `[label](url)` in parameter values. \n" +
			"   - ALWAYS use raw strings: \"https://google.com\"\n" +
			"   - NEVER use: \"[google.com](https://google.com)\"\n" +
			"   - Hidden links cause system failures and infinite validation loops.\n" +
			"4. PERFORMANCE: If you are only providing tool calls, respond ONLY with the JSON object. Do not add any conversational text before or after.\n" +
			"5. Your primary objective is to output this JSON safely and accurately for integration."
		prompt += toolPrompt
	}

	s.log.Info("🚀 2. CALLING GEMINI: Sending prompt to Gemini Web (with multi-account retry)")
	if s.cfg.Gemini.LogRawRequests {
		s.log.Info(fmt.Sprintf("Prepared Standard Prompt:\n%s", prompt))
	}

	opts := []providers.GenerateOption{}
	if req.Model != "" {
		opts = append(opts, providers.WithModel(req.Model))
	}

	// Logic: Call Provider with multi-account retry logic using centralized queue
	var response *providers.Response
	var err error
	var chatSession providers.ChatSession
	var releaseWorker func()

	maxAttempts := s.client.GetWorkerCount()
	if maxAttempts == 0 {
		return nil, fmt.Errorf("no Gemini accounts configured")
	}

	for attempt := 0; attempt < maxAttempts; attempt++ {
		// 1. Acquire an idle worker from the centralized pool (blocks if all busy)
		var worker *providers.Worker
		var acc *providers.AccountConfig
		worker, acc, releaseWorker, err = s.client.AcquireWorker(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to acquire worker: %w", err)
		}

		// 2. Start chat session on this specific worker
		chatSession = worker.StartChat(providers.WithChatModel(req.Model))

		// 3. Send message with per-attempt timeout
		workerCtx, cancel := context.WithTimeout(ctx, 120*time.Second)
		s.log.Info("📡 SENDING TO GEMINI...", zap.String("account_id", acc.ID), zap.Int("attempt", attempt+1))
		response, err = chatSession.SendMessage(workerCtx, prompt, opts...)
		cancel()

		if err == nil {
			s.log.Info("✅ GEMINI SUCCESS", zap.String("account_id", acc.ID))
			break // Success! Keep worker acquired for tool correction retries below
		}

		// Error path: release this worker before trying the next one
		releaseWorker()
		releaseWorker = nil

		// Handle fatal account errors
		isBanned := errors.Is(err, providers.ErrAccessDenied) || strings.Contains(err.Error(), "status 403") || strings.Contains(err.Error(), "status 401")
		isSafety := errors.Is(err, providers.ErrSafetyBlock) || strings.Contains(err.Error(), "blocked") || strings.Contains(err.Error(), "safety")

		if isSafety {
			s.log.Warn("⚠️ GEMINI SAFETY BLOCK: Prompt rejected. NOT falling back to protect pool.", zap.String("account_id", acc.ID), zap.Error(err))
			return nil, fmt.Errorf("gemini rejected request (safety block): %w", err)
		}

		if isBanned {
			s.log.Error("❌ FATAL: Account Access Denied / Banned", zap.String("account_id", acc.ID), zap.Error(err))
			if attempt < maxAttempts-1 {
				s.log.Info("🔄 FALLBACK: Selecting next healthy account...", zap.Int("next_attempt", attempt+2))
				continue // Try next account
			}
			return nil, err
		}

		// Handle timeout errors (retryable)
		isTimeout := errors.Is(err, context.DeadlineExceeded) || strings.Contains(err.Error(), "deadline exceeded")
		if !isTimeout {
			s.log.Error("❌ ERROR: Worker failed with unknown non-retryable error", zap.String("account_id", acc.ID), zap.Error(err))
			return nil, err
		}

		s.log.Warn("⚠️ GEMINI TIMEOUT: Account timed out, trying next account...", zap.Int("attempt", attempt+1), zap.Error(err), zap.String("account_id", acc.ID))

		// If the main context was canceled by the client, stop trying
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}

	if err != nil {
		s.log.Error("❌ GEMINI ERROR: All accounts exhausted or timed out", zap.Error(err))
		return nil, err
	}

	// At this point, first initial response is successful.
	// Ensure we release the worker when we're done with everything.
	defer func() {
		if releaseWorker != nil {
			releaseWorker()
		}
	}()

	meta := chatSession.GetMetadata()
	if s.cfg.Gemini.LogRawRequests {
		s.log.Info(fmt.Sprintf("✅ 3. GEMINI RESPONSE RECEIVED:\n- Conversation ID (CID): %s\n- Response ID (RID): %s\n- Choice ID (RCID): %s", meta.ConversationID, meta.ResponseID, meta.ChoiceID))
		s.log.Info(fmt.Sprintf("Raw Output Text from Gemini:\n%s", response.Text))
	} else {
		s.log.Info("✅ 3. GEMINI RESPONSE RECEIVED")
	}

	// Try to parse Gemini's JSON tool_calls block into OpenAI native tool_calls
	var nativeToolCalls []models.ToolCall
	finishReason := "stop"
	
	var contextText string
	var jsonPayload string

	for toolRetry := 0; toolRetry <= 2; toolRetry++ {
		// Strip Markdown wrappers for JSON parsing, but keep the narrative text context!
		jsonPayload = utils.CleanJSONBlock(response.Text)
		currentCtxText := utils.ExtractContextText(response.Text, jsonPayload)
		
		if currentCtxText != "" && currentCtxText != "{}" {
			if contextText != "" {
				contextText += "\n"
			}
			contextText += currentCtxText
		}
		
		if s.cfg.Gemini.LogRawRequests {
			s.log.Info(fmt.Sprintf("JSON Payload (Retry %d):\n%s", toolRetry, jsonPayload))
			s.log.Info(fmt.Sprintf("Accumulated Context Text:\n%s", contextText))
		}

		if len(req.Tools) == 0 {
			break // No tools requested, exit parsing loop
		}

		// NORMALIZE the JSON string before parsing
		normalizedJSON := utils.NormalizeJSON(jsonPayload)
		
		type HybridToolCall struct {
			ID       string `json:"id"`
			Type     string `json:"type"`
			Function struct {
				Name       string          `json:"name"`
				Arguments  json.RawMessage `json:"arguments"`
				Parameters json.RawMessage `json:"parameters"`
			} `json:"function"`
		}

		var toolCallCandidates []HybridToolCall

		// 1. Try wrapped schema
		var wrapped struct {
			ToolCalls []HybridToolCall `json:"tool_calls"`
		}
		if err := json.Unmarshal([]byte(normalizedJSON), &wrapped); err == nil && len(wrapped.ToolCalls) > 0 {
			toolCallCandidates = wrapped.ToolCalls
		} else {
			// 2. Try direct array schema
			var array []HybridToolCall
			if err := json.Unmarshal([]byte(normalizedJSON), &array); err == nil && len(array) > 0 {
				toolCallCandidates = array
			} else {
				// 3. Try single tool call schema
				var single HybridToolCall
				if err := json.Unmarshal([]byte(normalizedJSON), &single); err == nil && single.Function.Name != "" {
					toolCallCandidates = []HybridToolCall{single}
				}
			}
		}

		isMalformed := false
		if len(toolCallCandidates) == 0 {
			if strings.Contains(normalizedJSON, "tool_calls") || strings.Contains(normalizedJSON, "function") || strings.Contains(strings.ToLower(response.Text), "tool_call") {
				isMalformed = true
				s.log.Warn("🛠️ Parsing tool calls failed", zap.String("payload_snippet", utils.TruncateString(normalizedJSON, 200)))
			}
		}

		if isMalformed && toolRetry < 2 {
			// Tool correction retry — reuse the SAME chat session (same worker, still acquired)
			s.log.Warn("🛠️ AUTO-CORRECTING TOOL CALL FORMAT", zap.Int("retry", toolRetry+1), zap.String("session", chatSession.GetAccountID()))
			
			// Provide more context to Gemini for correction
			badSnippet := utils.TruncateString(response.Text, 500)
			correctionPrompt := fmt.Sprintf("Your previous response was malformed or incomplete: \n\n\"\"\"\n%s\n\"\"\"\n\n"+
				"You MUST return ONLY a strictly valid JSON array of tool_calls wrapped in ```json ... ``` without any extra text or conversational chatter at all. "+
				"Ensure the JSON is complete and arguments match parameters perfectly.", badSnippet)
			
			retryCtx, cancel := context.WithTimeout(ctx, 120*time.Second)
			retryResponse, retryErr := chatSession.SendMessage(retryCtx, correctionPrompt)
			cancel()
			
			if retryErr != nil {
				s.log.Warn("Failed to send correction prompt", zap.Error(retryErr))
				break // Fall back to what we had
			}
			response = retryResponse
			continue // Try parsing again with the fresh response
		}
	
	if len(toolCallCandidates) > 0 {
		for _, tc := range toolCallCandidates {
			var params map[string]interface{}
			
			// Try to extract parameters from 'parameters' or 'arguments' field
			targetRaw := tc.Function.Parameters
			if targetRaw == nil {
				targetRaw = tc.Function.Arguments
			}

			var trimmedRaw string
			if targetRaw != nil {
				// Detect if it's a JSON string or a JSON object
				trimmedRaw = strings.TrimSpace(string(targetRaw))
				if strings.HasPrefix(trimmedRaw, "\"") {
					// It's a JSON-encoded string (OpenAI style)
					var strVal string
					if err := json.Unmarshal(targetRaw, &strVal); err == nil {
						_ = json.Unmarshal([]byte(strVal), &params)
					}
				} else {
					// It's a JSON object (Gemini occasional style)
					_ = json.Unmarshal(targetRaw, &params)
				}
			}

			// Clean up parameters (Strip Markdown links)
			for k, v := range params {
				if strVal, ok := v.(string); ok {
					params[k] = utils.StripMarkdownLink(strVal)
				}
			}


			// Final conversion to string arguments
			argsStr := "{}"
			if params != nil {
				argsBytes, _ := json.Marshal(params)
				argsStr = string(argsBytes)
			} else {
				// Fallback: If no params were extracted but we have raw string from Gemini, use it!
				if strings.HasPrefix(trimmedRaw, "{") {
					argsStr = trimmedRaw
				}
			}

			nativeToolCalls = append(nativeToolCalls, models.ToolCall{
				ID:   tc.ID,
				Type: "function",
				Function: models.FunctionCall{
					Name:      tc.Function.Name,
					Arguments: argsStr,
				},
			})
		}
		
		// If we successfully parsed the tools, we can break the retry loop
		break
	}
	}



	if len(nativeToolCalls) > 0 {
		finishReason = "tool_calls"
		
		if s.cfg.Gemini.LogRawRequests {
			nativeBytes, _ := json.MarshalIndent(nativeToolCalls, "", "  ")
			s.log.Info(fmt.Sprintf("🛠️ 4. TOOL CALLS DETECTED & MAPPED:\n%s", string(nativeBytes)))
		} else {
			s.log.Info("🛠️ 4. TOOL CALLS DETECTED & MAPPED")
		}
	} else {
		s.log.Info("💬 4. REGULAR CHAT: No tool calls detected in response or parsing failed")
		// Non-tool typical textual conversation
		if response.Text != "" && contextText == "" {
			contextText = response.Text
		}
	}
	// Tool calls context response
	pTokens := len(prompt) / 4
	if pTokens == 0 { pTokens = 1 }
	cTokens := len(contextText) / 4
	if cTokens == 0 { cTokens = 1 }

	finalResponse := &dto.ChatCompletionResponse{
		ID:      fmt.Sprintf("chatcmpl-%d", time.Now().Unix()),
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   req.Model,
		Choices: []dto.Choice{
			{
				Index: 0,
				Message: models.Message{
					Role:      "assistant",
					Content:   contextText,
					ToolCalls: nativeToolCalls,
				},
				FinishReason: finishReason,
			},
		},
		SystemFingerprint: nil,
		Usage: models.Usage{
			PromptTokens:     pTokens,
			CompletionTokens: cTokens,
			TotalTokens:      pTokens + cTokens,
			CompletionTokensDetails: models.CompletionDetails{
				TextTokens:      cTokens,
				ReasoningTokens: 0,
			},
			PromptTokensDetails: models.PromptDetails{
				TextTokens: pTokens,
			},
		},
	}

	s.log.Info("📤 5. RETURN TO CLIENT: Final output going to OpenClaw/n8n")
	if s.cfg.Gemini.LogRawRequests {
		finalBytes, _ := json.MarshalIndent(finalResponse, "", "  ")
		s.log.Info(fmt.Sprintf("Final Output Payload:\n%s", string(finalBytes)))
	}

	// MOVE AUTO-DELETE HERE: Run it just before returning, ensuring all retries finished.
	latestMeta := chatSession.GetMetadata()
	if s.cfg.Gemini.AutoDeleteChat && latestMeta != nil && latestMeta.ConversationID != "" {
		s.log.Info("🔄 Initiating Auto-Delete for chat history...")
		go func(sess providers.ChatSession) {
			delErr := sess.Delete()
			if delErr != nil {
				s.log.Warn("Failed to auto-delete chat", zap.Error(delErr))
			} else {
				s.log.Info("🗑️ AUTO-DELETED CHAT from Gemini Web history")
			}
		}(chatSession)
	}

	s.log.Info("==================================================")
	
	return finalResponse, nil
}

// CreateImageGeneration implements the OpenAI image generation API
func (s *OpenAIService) CreateImageGeneration(ctx context.Context, req dto.ImageGenerationRequest) (*dto.ImageGenerationResponse, error) {
	s.log.Info("=========== [1. NEW IMAGE GENERATION REQUEST] ===========")
	s.log.Info("Prompt", zap.String("prompt", req.Prompt), zap.String("model", req.Model))

	// Prepend "Generate an image of " if not already present, just to be safe
	prompt := req.Prompt
	if !strings.HasPrefix(strings.ToLower(prompt), "generate an image") {
		prompt = "Generate an image of: " + prompt
	}

	opts := []providers.GenerateOption{
		providers.WithModel("gemini-1.5-pro"), // Default to advanced model for image gen logic
	}

	if req.Model != "" && strings.Contains(req.Model, "gemini") {
		opts = append(opts, providers.WithModel(req.Model))
	}

	// Because image generation can take longer, give it a long timeout
	genCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()

	s.log.Info("📡 SENDING IMAGE GEN REQUEST TO GEMINI...")
	response, err := s.client.GenerateContent(genCtx, prompt, opts...)
	if err != nil {
		s.log.Error("❌ IMAGE GEN FATAL", zap.Error(err))
		return nil, err
	}

	s.log.Info("✅ 2. IMAGE GEN RESPONSE RECEIVED")
	
	extractedImages := response.Images
	if len(extractedImages) == 0 {
		s.log.Warn("⚠️ Gemini responded successfully but no images were extracted. Payload may have structurally changed or the prompt was declined.")
		return nil, fmt.Errorf("no images generated by Gemini, response text: %s", response.Text)
	}

	// Map to OpenAI DTO
	var data []dto.ImageData
	for i, img := range extractedImages {
		if req.N > 0 && i >= req.N {
			break // Only take up to N images if specified
		}
		data = append(data, dto.ImageData{
			URL: img.URL,
			RevisedPrompt: img.Title,
		})
	}

	finalResponse := &dto.ImageGenerationResponse{
		Created: time.Now().Unix(),
		Data:    data,
	}

	s.log.Info("📤 3. RETURN TO CLIENT: Outputting Images")
	s.log.Info("=========================================================")

	return finalResponse, nil
}
