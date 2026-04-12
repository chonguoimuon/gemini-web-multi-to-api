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
	pingPongIdleTimeout = 5 * time.Minute
)

// sendWithPingPong sends a message with an idle timeout that resets on each received data chunk.
//
// Unlike context.WithTimeout (which kills at a fixed deadline regardless of activity),
// this implements ping-pong behaviour:
//   - Each incoming data chunk resets the idle timer
//   - If no data arrives within idleTimeout, the connection is cancelled (real hang)
//   - maxTimeout is governed by the parent ctx (e.g., 30 minutes global ceiling)
func sendWithPingPong(
	ctx context.Context,
	session providers.ChatSession,
	message string,
	extraOpts ...providers.GenerateOption,
) (*providers.Response, error) {
	// Idle context — cancelled if no data flows within idleTimeout
	idleCtx, idleCancel := context.WithCancel(ctx)
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

	// Inject universal proxy constraints to prevent Gemini from generating unparseable frontend UI components
	prompt += "\n\n[USER ENVIRONMENT CONSTRAINT]\n1. Strictly avoid using any code execution environments, canvas-based tools, or rendering.\n2. Disable all interactive UI components, including 'Immersive Entry Chips,' dynamic cards, or external media links.\n3. Provide all output exclusively in raw Markdown and structured JSON.\n4. Do not generate any system-invoked interactive elements."

	// Inject tool instruction if requested
	if len(req.Tools) > 0 {
		toolPrompt := "\n\n[STRICT TOOL CALLING MODE]\n" +
			"If you need to use a tool to fulfill the request, you MUST provide the tool call instructions as a JSON array named `tool_calls`.\n" +
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
			"1. If no tool is needed, respond with natural text only.\n" +
			"2. If tools ARE needed, you MUST include a brief natural text explanation BEFORE the JSON block. Do not leave the content empty.\n" +
			"3. All URLs in tool arguments MUST be raw strings (e.g., https://google.com). NEVER use Markdown formatting for URLs inside JSON.\n" +
			"4. CRITICAL: The `tool_calls` block must be valid JSON and properly closed.\n" +
			"5. Conversational text MUST be outside the JSON block."
		prompt += toolPrompt
	}

	if s.cfg.Gemini.LogRawSendGemini {
		s.log.Info(fmt.Sprintf("\n========== PROMPT SENT TO GEMINI ==========\n%s\n===========================================", prompt))
	}

	s.log.Info("🚀 2. CALLING GEMINI: Sending prompt to Gemini Web (with multi-account retry)")

	// ─── CHUNKING LOGIC (V18): Prevent bans by splitting large prompts ──────────
	packSize := s.cfg.Gemini.PackContext
	if packSize <= 0 {
		packSize = 32000
	}

	// Use rune count for approximation of context size as requested
	if len([]rune(prompt)) > packSize {
		s.log.Info("📦 Prompt exceeds PackContext threshold. Triggering Multi-Packet Chunking flow.",
			zap.Int("length", len([]rune(prompt))),
			zap.Int("threshold", packSize))
		return s.handleChunkedCompletion(ctx, req, prompt, packSize)
	}

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

	// ─── FINAL ASSEMBLY & REFINEMENT ──────────────────────────────────────────
	// Logic lọc tool calls và sửa lỗi tập trung hoàn toàn trong assembleResponse
	return s.assembleResponse(ctx, req, response, chatSession, releaseWorker, prompt, opts, triedAccounts)
}

func (s *OpenAIService) handleChunkedCompletion(ctx context.Context, req dto.ChatCompletionRequest, prompt string, packSize int) (*dto.ChatCompletionResponse, error) {
	chunks := utils.SplitStringChunks(prompt, packSize)
	numChunks := len(chunks)

	preamble := fmt.Sprintf("I will send you a long content split into %d packets. For each packet, please simply reply with \"next\" to acknowledge receipt. Do NOT summarize or respond until I have sent all packets. Once I send the last packet, you should synthesize everything and provide a final response according to the original instructions. Do you understand?", numChunks)

	opts := []providers.GenerateOption{}
	if req.Model != "" {
		opts = append(opts, providers.WithModel(req.Model))
	}

	triedAccounts := make(map[string]bool)
	var lastErr error

	// Try with primary accounts then fallback to guest
	for attempt := 0; attempt < s.cfg.Gemini.MaxRetries+2; attempt++ {
		var worker *providers.Worker
		var acc *providers.AccountConfig
		var release func()
		var err error

		worker, acc, release, err = s.client.AcquireWorkerExcluding(ctx, triedAccounts)
		if err != nil {
			// Try guest
			worker, acc, release, err = s.client.AcquireGuestWorker(ctx)
			if err != nil {
				break // No more options
			}
		}

		triedAccounts[acc.ID] = true
		session := worker.StartChat(providers.WithChatModel(req.Model))

		cleanupOnFailure := func() {
			accID := session.GetAccountID()
			if !strings.Contains(accID, "guest-") {
				go func() { _ = session.Delete() }()
			}
			release()
		}

		s.log.Info("📡 Starting Multi-Packet Handshake", zap.String("account_id", acc.ID))
		resp, err := sendWithPingPong(ctx, session, preamble, opts...)
		if err != nil {
			s.log.Warn("❌ Handshake failed", zap.String("account_id", acc.ID), zap.Error(err))
			cleanupOnFailure()
			lastErr = err
			continue
		}
		s.log.Info("🏓 ping-pong (Handshake confirmed)", zap.String("account_id", acc.ID))

		// Send chunks 1..numChunks-1
		failed := false
		for i := 0; i < numChunks-1; i++ {
			chunkMsg := fmt.Sprintf("[Packet %d/%d]\n\n%s\n\nPlease reply with \"next\".", i+1, numChunks, chunks[i])
			s.log.Info(fmt.Sprintf("📦 [Gửi gói %d/%d]", i+1, numChunks), zap.String("account_id", acc.ID))

			resp, err = sendWithPingPong(ctx, session, chunkMsg, opts...)
			if err != nil {
				s.log.Warn(fmt.Sprintf("❌ Packet %d failed", i+1), zap.String("account_id", acc.ID), zap.Error(err))
				failed = true
				break
			}

			lower := strings.ToLower(resp.Text)
			if strings.Contains(lower, "next") || strings.Contains(lower, "ok") || strings.Contains(lower, "understand") {
				s.log.Info("🏓 ping-pong", zap.Int("packet", i+1))
			}
		}

		if failed {
			cleanupOnFailure()
			continue
		}

		// Final chunk
		s.log.Info(fmt.Sprintf("📦 [Gửi gói %d/%d (FINAL)]", numChunks, numChunks), zap.String("account_id", acc.ID))
		finalMsg := fmt.Sprintf("[Packet %d/%d (FINAL)]\n\n%s\n\nAll packets have been sent. Please now synthesize the information and provide your final response.", numChunks, numChunks, chunks[numChunks-1])

		resp, err = sendWithPingPong(ctx, session, finalMsg, opts...)
		if err != nil {
			s.log.Warn("❌ Final packet failed", zap.String("account_id", acc.ID), zap.Error(err))
			cleanupOnFailure()
			lastErr = err
			continue
		}

		// Re-use standard refining logic to extract tools/content
		return s.assembleResponse(ctx, req, resp, session, release, prompt, opts, triedAccounts)
	}

	if lastErr != nil {
		return nil, fmt.Errorf("multi-packet upload failed: %w", lastErr)
	}
	return nil, fmt.Errorf("multi-packet upload failed: all accounts exhausted")
}

// assembleResponse internalizes the refinement loop and OpenAI payload construction
func (s *OpenAIService) assembleResponse(
	ctx context.Context,
	req dto.ChatCompletionRequest,
	initialResp *providers.Response,
	chatSession providers.ChatSession,
	releaseWorker func(),
	originalPrompt string,
	opts []providers.GenerateOption,
	triedAccounts map[string]bool,
) (*dto.ChatCompletionResponse, error) {
	var finalContent string
	var finalToolCalls []models.ToolCall
	response := initialResp

	// Extract standard refining parameters from main logic
	if s.cfg.Gemini.LogRawGeminiOut && response != nil {
		s.log.Info(fmt.Sprintf("\n========== RAW GEMINI RESPONSE (Initial) ==========\n%s\n===================================================", response.Text))
	}

	const immersiveChipURL = "http://googleusercontent.com/immersive_entry_chip/0"

	for retry := 0; retry < 5; retry++ {
		if response == nil {
			break
		}
		rawText := response.Text
		hadImmersiveChip := strings.Contains(rawText, immersiveChipURL)
		if hadImmersiveChip {
			rawText = strings.ReplaceAll(rawText, immersiveChipURL, "")
		}

		jsonBlocks, sourceSegments := utils.ExtractAllJSONBlocks(rawText)
		finalContent = utils.ExtractContextText(rawText, sourceSegments)

		var currentToolCalls []models.ToolCall
		isValid := true

		// Flexible tool parsing (V18 logic)
		for _, block := range jsonBlocks {
			var rawToolCalls []interface{}
			var mapWrapper map[string]interface{}
			if err := json.Unmarshal([]byte(block), &mapWrapper); err == nil {
				if tc, ok := mapWrapper["tool_calls"].([]interface{}); ok {
					rawToolCalls = tc
				}
			}
			if len(rawToolCalls) == 0 {
				var listWrapper []interface{}
				if err := json.Unmarshal([]byte(block), &listWrapper); err == nil {
					rawToolCalls = listWrapper
				}
			}

			if len(rawToolCalls) > 0 {
				var tempCalls []models.ToolCall
				for _, rtc := range rawToolCalls {
					tcMap, ok := rtc.(map[string]interface{})
					if !ok {
						continue
					}
					fnMap, ok := tcMap["function"].(map[string]interface{})
					if !ok {
						continue
					}
					name, _ := fnMap["name"].(string)
					argsRaw := fnMap["arguments"]

					// --- PHẦN VÁ LỖI ARGUMENTS ---
					var finalArgs string
					switch v := argsRaw.(type) {
					case string:
						// Nếu là string, kiểm tra xem nó có phải JSON hợp lệ không
						if json.Valid([]byte(v)) {
							finalArgs = v
						} else {
							// Nếu là text thường, ép về JSON string hợp lệ
							b, _ := json.Marshal(v)
							finalArgs = string(b)
						}
					case map[string]interface{}, []interface{}:
						// Nếu Gemini trả về Object/Array trực tiếp, Marshal thành string
						b, _ := json.Marshal(v)
						finalArgs = string(b)
					default:
						finalArgs = "{}"
					}

					// --- PHẦN VÁ LỖI ID & TYPE ---
					id, _ := tcMap["id"].(string)
					if id == "" {
						// Sử dụng UnixNano để tránh xung đột ID trong Roo Code
						id = fmt.Sprintf("call_%d", time.Now().UnixNano())
					}

					typ, _ := tcMap["type"].(string)
					if typ == "" || typ == "tool_call_partial" {
						typ = "function"
					}

					if name != "" {
						tempCalls = append(tempCalls, models.ToolCall{
							ID:   id,
							Type: typ,
							Function: models.FunctionCall{
								Name:      name,
								Arguments: finalArgs,
							},
						})
					}
				}
				if len(tempCalls) > 0 {
					currentToolCalls = tempCalls
					isValid = true
					break
				}
			}
		}

		// Validation check for Immersive View truncation, junk, or malformed tool_calls
		shouldRetry := false
		fixPrompt := ""

		if strings.Contains(response.Text, immersiveChipURL) && len(currentToolCalls) == 0 && strings.Contains(rawText, "\"tool_calls\"") {
			shouldRetry = true
			fixPrompt = "Your previous response was interrupted by an Immersive View component mid-stream. Please CONTINUE the response exactly where it was cut off."
		} else if utils.ContainsJunkIndicators(rawText) {
			shouldRetry = true
			fixPrompt = "Response contains prohibited interactive UI components. Please provide a clean response in raw Markdown and structured JSON."
		} else if len(currentToolCalls) == 0 && (strings.Contains(rawText, "\"tool_calls\"") || strings.Contains(rawText, "\"function\"")) {
			// Đây là trường hợp quan trọng: Gemini có ý định gọi tool nhưng proxy không parse được JSON
			shouldRetry = true
			fixPrompt = "nội dung tool_calls của bạn trong tin nhắn trước bị lỗi định dạng yêu cầu đọc lại các prompt hướng dẩn sử dụng đã cung cấp và tuân thủ"
		} else if len(currentToolCalls) > 0 {
			for _, tc := range currentToolCalls {
				if utils.HasMarkdownLink(tc.Function.Arguments) {
					isValid = false
					fixPrompt = "Your previous tool_calls contained invalid Markdown links in arguments. Please provide raw string URLs only."
					break
				}
			}
		}

		if !isValid || shouldRetry {
			s.log.Warn("⚠️ Response invalid or truncated, attempting self-correction...", zap.Int("retry", retry+1))

			if fixPrompt == "" {
				fixPrompt = "Your previous response was malformed. Please provide the COMPLETE and CORRECT [tool_calls] JSON block now."
			}

			// Nested worker-swap retry logic (preservation of conversation context)
			maxSwapRetries := 2
			for swapAttempt := 0; swapAttempt <= maxSwapRetries; swapAttempt++ {
				var err error
				response, err = sendWithPingPong(ctx, chatSession, fixPrompt, opts...)
				if err == nil {
					break
				}

				if swapAttempt == maxSwapRetries {
					return nil, err
				}

				// Preservation of conversation context on account swap
				meta := chatSession.GetMetadata()
				if releaseWorker != nil {
					releaseWorker()
				}

				var newWorker *providers.Worker
				var newAcc *providers.AccountConfig
				newWorker, newAcc, releaseWorker, err = s.client.AcquireWorkerExcluding(ctx, triedAccounts)
				if err != nil {
					return nil, err
				}
				triedAccounts[newAcc.ID] = true
				chatSession = newWorker.StartChat(providers.WithChatModel(req.Model), providers.WithChatMetadata(meta))
			}

			if hadImmersiveChip && response != nil {
				continuation := strings.TrimSpace(response.Text)
				continuation = strings.TrimPrefix(continuation, "```json")
				continuation = strings.TrimPrefix(continuation, "```")
				continuation = strings.TrimSuffix(continuation, "```")
				response.Text = strings.TrimRight(rawText, " \t\n\r") + strings.TrimSpace(continuation)
			}
			continue
		}

		finalToolCalls = currentToolCalls
		break
	}

	// Assembly
	finishReason := "stop"
	if len(finalToolCalls) > 0 {
		finishReason = "tool_calls"
	}
	finalChoices := []dto.Choice{
		{
			Index: 0,
			Message: models.Message{
				Role:    "assistant",
				Content: finalContent,
				//Type:      "text",
				ToolCalls: finalToolCalls,
			},
			FinishReason: finishReason,
		},
	}

	// Final content assignment to ensure UI visibility in tools like Roo Code
	if finalContent == "" {
		// If clean extraction failed, use raw response text as fallback
		finalChoices[0].Message.Content = response.Text
	} else {
		finalChoices[0].Message.Content = finalContent
	}

	// Double-safety: if still empty but tools exist, provide a status message
	//if finalChoices[0].Message.Content == "" && len(finalToolCalls) > 0 {
	//	finalChoices[0].Message.Content = "I am calling tools to assist with your request..."
	//}

	// Logic: Precise token counting based on the ACTUAL payload being returned
	finalChoicesMsg := finalChoices[0].Message
	toolCallsJSON, _ := json.Marshal(finalChoicesMsg.ToolCalls)

	pTokens := len([]rune(originalPrompt)) / 4
	cTokens := (len([]rune(finalChoicesMsg.Content)) + len([]rune(string(toolCallsJSON)))) / 4
	if cTokens == 0 && (len(finalChoicesMsg.Content) > 0 || len(finalChoicesMsg.ToolCalls) > 0) {
		cTokens = 1 // Ensure at least 1 token if there is output
	}

	finalResponse := &dto.ChatCompletionResponse{
		ID:      fmt.Sprintf("chatcmpl-%d", time.Now().Unix()),
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   req.Model,
		Choices: finalChoices,
		Usage: models.Usage{
			PromptTokens: pTokens, CompletionTokens: cTokens, TotalTokens: pTokens + cTokens,
		},
		SystemFingerprint: "fp_gemini_web_v1",
	}

	// Logic: Log the final payload if requested by the user
	if s.cfg.Gemini.LogRawPayloadOut {
		b, _ := json.MarshalIndent(finalResponse, "", "  ")
		s.log.Info(fmt.Sprintf("\n========== FINAL PAYLOAD OUT (OpenAI Format) ==========\n%s\n=======================================================", string(b)))
	}

	// Always prioritize cleanup for multi-turn or chunked uploads to prevent clutter
	// [OPTIMIZATION] Skip deletion for guest accounts as Google doesn't persist them
	latestMeta := chatSession.GetMetadata()
	accID := chatSession.GetAccountID()
	if !strings.Contains(accID, "guest-") && (s.cfg.Gemini.AutoDeleteChat || len(originalPrompt) > s.cfg.Gemini.PackContext) && latestMeta != nil && latestMeta.ConversationID != "" {
		go func(sess providers.ChatSession) { _ = sess.Delete() }(chatSession)
	}

	releaseWorker()
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
