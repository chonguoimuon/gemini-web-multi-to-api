package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"errors"
	"gemini-web-to-api/internal/commons/configs"
	"gemini-web-to-api/internal/commons/models"
	"gemini-web-to-api/internal/commons/utils"
	"gemini-web-to-api/internal/modules/openai/dto"
	"gemini-web-to-api/internal/modules/providers"

	"go.uber.org/zap"
)

type OpenAIService struct {
	client *providers.Client
	log    *zap.Logger
	cfg    *configs.Config
}

const (
	// pingPongIdleTimeout: if no data chunk received within this duration, the connection is killed.
	// This implements "ping-pong" keeping the connection alive during active streaming
	// but detecting real hangs where Gemini stops sending data.
	pingPongIdleTimeout = 120 * time.Second

	// pingPongMaxTimeout: absolute maximum request duration regardless of streaming activity.
	// Prevents infinite hangs on extremely long responses.
	pingPongMaxTimeout = 5 * time.Minute
)

// sendWithPingPong sends a message with an idle timeout that resets on each received data chunk.
//
// Unlike context.WithTimeout (which kills at a fixed deadline regardless of activity),
// this implements ping-pong behaviour:
//   - Each incoming data chunk resets the idle timer
//   - If no data arrives within idleTimeout, the connection is cancelled (real hang)
//   - maxTimeout is an absolute upper bound to prevent infinite streaming
func sendWithPingPong(
	ctx context.Context,
	session providers.ChatSession,
	message string,
	extraOpts ...providers.GenerateOption,
) (*providers.Response, error) {
	// Absolute max context — hard ceiling
	absCtx, absCancel := context.WithTimeout(ctx, pingPongMaxTimeout)
	defer absCancel()

	// Idle context — cancelled if no data flows within idleTimeout
	idleCtx, idleCancel := context.WithCancel(absCtx)
	defer idleCancel()

	idleTimer := time.NewTimer(pingPongIdleTimeout)
	defer idleTimer.Stop()

	// Indicator if timeout was triggered
	idleExpired := false

	// Background goroutine: fires idleCancel() if timer expires (no data received)
	go func() {
		select {
		case <-idleTimer.C:
			idleExpired = true
			idleCancel() // Idle timeout — kill connection
		case <-idleCtx.Done():
			// Already cancelled (success path or abs timeout)
		}
	}()

	// OnProgress resets idle timer each time a data chunk arrives — this is the "pong"
	pingPongOpt := providers.WithProgress(func() {
		idleTimer.Reset(pingPongIdleTimeout)
	})

	allOpts := append(extraOpts, pingPongOpt)
	resp, err := session.SendMessage(idleCtx, message, allOpts...)
	if err != nil && idleExpired {
		return nil, fmt.Errorf("idle timeout: no data received from Gemini within %v", pingPongIdleTimeout)
	}
	return resp, err
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

	// Inject tool instruction if requested
	if len(req.Tools) > 0 {
		toolPrompt := "\n\n[STRICT TOOL CALLING MODE]\n" +
			"If you need to use any tools, you MUST respond with a JSON object containing a `tool_calls` array at the end of your response.\n" +
			"Example format:\n" +
			"```json\n" +
			"{\n" +
			"  \"tool_calls\": [\n" +
			"    {\n" +
			"      \"id\": \"call_abc123\",\n" +
			"      \"type\": \"function\",\n" +
			"      \"function\": {\n" +
			"        \"name\": \"function_name\",\n" +
			"        \"arguments\": \"{\\\"arg1\\\": \\\"val1\\\"}\"\n" +
			"      }\n" +
			"    }\n" +
			"  ]\n" +
			"}\n" +
			"```\n" +
			"IMPORTANT RULES:\n" +
			"1. The `tool_calls` field MUST be a JSON array. Never return it as a string.\n" +
			"2. The `arguments` field MUST be a stringified JSON object.\n" +
			"3. If you need to clarify something or ask the user for more information, you MUST use the `follow_up` tool if available.\n" +
			"4. CRITICAL: All URLs in tool arguments MUST be raw strings (e.g., https://google.com). NEVER use Markdown formatting like [label](url).\n" +
			"5. If providing tool calls, do not add unnecessary conversational text if possible, or keep it separate from the JSON block."
		prompt += toolPrompt
	}

	s.log.Info("🚀 2. CALLING GEMINI: Sending prompt to Gemini Web (with multi-account retry)")

	opts := []providers.GenerateOption{}
	if req.Model != "" {
		opts = append(opts, providers.WithModel(req.Model))
	}

	var response *providers.Response
	var err error
	var chatSession providers.ChatSession
	var releaseWorker func()

	maxRetries := s.cfg.Gemini.MaxRetries
	if maxRetries <= 0 {
		maxRetries = 3
	}

	var lastErr error
	var useGuestFallback bool
	triedAccounts := make(map[string]bool)

	for attempt := 0; attempt < maxRetries; attempt++ {
		var worker *providers.Worker
		var acc *providers.AccountConfig

		// AcquireWorkerExcluding is NON-BLOCKING: returns immediately if no fresh account available
		worker, acc, releaseWorker, err = s.client.AcquireWorkerExcluding(ctx, triedAccounts)
		if errors.Is(err, providers.ErrAllAccountsExhausted) || err != nil {
			s.log.Info("📭 No more unused accounts available, falling back to guest", zap.Int("tried", len(triedAccounts)))
			useGuestFallback = true
			break
		}

		triedAccounts[acc.ID] = true
		chatSession = worker.StartChat(providers.WithChatModel(req.Model))
		s.log.Info("📡 SENDING TO GEMINI (ping-pong idle=120s, max=5min)...", zap.String("account_id", acc.ID), zap.Int("attempt", attempt+1))
		response, err = sendWithPingPong(ctx, chatSession, prompt, opts...)

		if err == nil {
			s.log.Info("✅ GEMINI SUCCESS", zap.String("account_id", acc.ID))
			break
		}

		// Release worker before deciding next step
		releaseWorker()
		releaseWorker = nil
		lastErr = err

		// Classify error type to decide routing
		isBanned := errors.Is(err, providers.ErrAccessDenied) ||
			errors.Is(err, providers.ErrBotBlocked) ||
			strings.Contains(err.Error(), "403") ||
			strings.Contains(err.Error(), "429")

		isTimeout := errors.Is(err, context.DeadlineExceeded) ||
			strings.Contains(err.Error(), "timeout") ||
			strings.Contains(err.Error(), "deadline exceeded") ||
			strings.Contains(err.Error(), "context deadline")

		if isBanned {
			// Ban/config error: trigger clearbot for THIS account, do NOT try others
			s.log.Info("🚨 Account BANNED/config error — triggering clearbot, moving directly to guest",
				zap.String("account_id", acc.ID), zap.Error(err))
			s.client.TriggerBotClearance(acc.ID)
			useGuestFallback = true
			break
		}

		if isTimeout {
			s.log.Info("⏱️ Account timeout — trying next available account",
				zap.String("account_id", acc.ID), zap.Int("attempt", attempt+1))
			// Loop continues, triedAccounts ensures a different account is picked
			continue
		}

		// Other errors: treat as timeout (try next account)
		s.log.Info("⚠️ Account error — trying next available account",
			zap.String("account_id", acc.ID), zap.Error(err))
		// Loop continues
	}

	// ─── GUEST FALLBACK: try exactly ONCE, no retry, no disable ───────────────
	if useGuestFallback || (err != nil && releaseWorker == nil) {
		s.log.Info("⚡ Executing Guest Fallback (Load Balanced, 1 attempt)...")
		var guestWorker *providers.Worker
		var guestAcc *providers.AccountConfig
		var releaseGuest func()
		guestWorker, guestAcc, releaseGuest, err = s.client.AcquireGuestWorker(ctx)
		if err != nil {
			if lastErr != nil {
				return nil, fmt.Errorf("all primary accounts failed (%v) and guest system is unavailable: %w", lastErr, err)
			}
			return nil, fmt.Errorf("guest system unavailable: %w", err)
		}
		chatSession = guestWorker.StartChat(providers.WithChatModel(req.Model))
		releaseWorker = releaseGuest
		s.log.Info("📡 SENDING TO GUEST (ping-pong idle=120s, max=5min)...", zap.String("account_id", guestAcc.ID))
		response, err = sendWithPingPong(ctx, chatSession, prompt, opts...)
		if err != nil {
			releaseWorker()
			// Report error to trigger re-learning but DO NOT disable the guest platform
			s.client.HandleGuestError(guestAcc.ID, err)
			return nil, fmt.Errorf("guest failure (re-learn queued): %w", err)
		}
		s.log.Info("✅ GUEST SUCCESS", zap.String("account_id", guestAcc.ID))
	} else if err != nil {
		return nil, err
	}

	defer func() {
		if releaseWorker != nil {
			releaseWorker()
		}
	}()

	// ─── TOOL CALL PROCESSING V13: Prompt-Driven & Correction Loop ─────────────
	var nativeToolCalls []models.ToolCall
	var contextText string
	finishReason := "stop"

	for retry := 0; retry < 2; retry++ {
		jsonBlocks, sourceSegments := utils.ExtractAllJSONBlocks(response.Text)
		contextText = utils.ExtractContextText(response.Text, sourceSegments)

		var currentToolCalls []models.ToolCall
		isValid := true
		foundAny := false

		for _, block := range jsonBlocks {
			var wrapper struct {
				ToolCalls interface{} `json:"tool_calls"`
			}
			if err := json.Unmarshal([]byte(block), &wrapper); err == nil && wrapper.ToolCalls != nil {
				foundAny = true
				// Strict check: must be an array
				rawTC, ok := wrapper.ToolCalls.([]interface{})
				if !ok {
					isValid = false
					s.log.Warn("⚠️ Gemini returned tool_calls as string/object instead of array", zap.String("account_id", response.Metadata["account_id"].(string)))
					break
				}

				// Re-unmarshal into concrete struct if it's an array
				var concreteTC []models.ToolCall
				tcBytes, _ := json.Marshal(rawTC)
				if err := json.Unmarshal(tcBytes, &concreteTC); err == nil {
					// V14: Check for Markdown Links in tool calls
					for _, tc := range concreteTC {
						if utils.HasMarkdownLink(tc.Function.Arguments) {
							isValid = false
							s.log.Warn("⚠️ Markdown Link detected in tool_calls arguments", zap.String("account_id", response.Metadata["account_id"].(string)))
							break
						}
					}
					if !isValid {
						break
					}
					currentToolCalls = append(currentToolCalls, concreteTC...)
				} else {
					isValid = false
					break
				}
			}
		}

		if foundAny && !isValid {
			s.log.Info("🔄 Correction Loop: Requesting Gemini to fix tool_calls/links format...", zap.Int("retry", retry+1))
			correctionPrompt := "Your previous response contained errors in tool_calls:\n" +
				"1. All tool_calls MUST be a JSON array, not a string.\n" +
				"2. All URLs in arguments MUST be raw strings. NEVER use Markdown links like [label](url).\n" +
				"Please correct and re-output the JSON structure now."

			// Send correction request to same chat session
			response, err = sendWithPingPong(ctx, chatSession, correctionPrompt, opts...)
			if err != nil {
				break // Stop on network error
			}
			continue // Try parsing again
		}

		// If we reached here, it's either valid or there were no tool calls
		nativeToolCalls = currentToolCalls
		break
	}

	if len(nativeToolCalls) > 0 {
		finishReason = "tool_calls"
	}

	// 5. Hollow Guard (V12)
	if len(nativeToolCalls) == 0 && contextText == "" {
		if response.Text != "" {
			contextText = response.Text
		} else {
			contextText = "... (Empty response) ..."
		}
	}

	// 6. Token calculation
	pTokens := len(prompt) / 4
	if pTokens == 0 {
		pTokens = 1
	}
	cTokens := len(contextText) / 4
	for _, tc := range nativeToolCalls {
		cTokens += len(tc.Function.Arguments) / 4
	}
	if cTokens == 0 {
		cTokens = 1
	}

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
		Usage: models.Usage{
			PromptTokens:     pTokens,
			CompletionTokens: cTokens,
			TotalTokens:      pTokens + cTokens,
			PromptTokensDetails: models.PromptDetails{
				TextTokens: pTokens,
			},
			CompletionTokensDetails: models.CompletionDetails{
				TextTokens:      cTokens,
				ReasoningTokens: 0,
			},
		},
		SystemFingerprint: "fp_gemini_web_v1",
	}

	// Auto-delete
	latestMeta := chatSession.GetMetadata()
	if s.cfg.Gemini.AutoDeleteChat && latestMeta != nil && latestMeta.ConversationID != "" {
		go func(sess providers.ChatSession) { _ = sess.Delete() }(chatSession)
	}

	if s.cfg.Gemini.LogRawRequests {
		resBytes, _ := json.MarshalIndent(finalResponse, "", "  ")
		s.log.Info(fmt.Sprintf("Response Payload:\n%s\nRaw Gemini text:\n%s", string(resBytes), response.Text))
	}

	s.log.Info("📤 RETURN TO CLIENT", zap.Int("tools", len(nativeToolCalls)))
	return finalResponse, nil
}

func (s *OpenAIService) CreateImageGeneration(ctx context.Context, req dto.ImageGenerationRequest) (*dto.ImageGenerationResponse, error) {
	s.log.Info("=========== [NEW IMAGE GENERATION REQUEST] ===========")
	prompt := req.Prompt
	if !strings.HasPrefix(strings.ToLower(prompt), "generate an image") {
		prompt = "Generate an image of: " + prompt
	}

	opts := []providers.GenerateOption{providers.WithModel("gemini-1.5-pro")}
	genCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()

	response, err := s.client.GenerateContent(genCtx, prompt, opts...)
	if err != nil {
		return nil, err
	}

	var data []dto.ImageData
	for i, img := range response.Images {
		if req.N > 0 && i >= req.N {
			break
		}
		data = append(data, dto.ImageData{URL: img.URL, RevisedPrompt: img.Title})
	}

	if len(data) == 0 {
		return nil, fmt.Errorf("no images generated: %s", response.Text)
	}

	return &dto.ImageGenerationResponse{
		Created: time.Now().Unix(),
		Data:    data,
	}, nil
}
