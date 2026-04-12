package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"
)

// GeminiChatSession implements ChatSession interface for Gemini
type GeminiChatSession struct {
	worker          *Worker // Refers to providers.Worker
	model           string
	metadata        *SessionMetadata
	history         []Message
	lastRawResponse string
}

// SendMessage sends a message in the chat session
func (s *GeminiChatSession) SendMessage(ctx context.Context, message string, options ...GenerateOption) (*Response, error) {
	// Read session token safely — short critical section, no lock held during HTTP call
	s.worker.mu.RLock()
	at := s.worker.at
	s.worker.mu.RUnlock()

	if at == "" && !strings.HasPrefix(s.worker.AccountID, "guest-") {
		return nil, fmt.Errorf("client not initialized")
	}
	if at == "" {
		at = "null" // Fallback for guest mode
	}

	mInfo, ok := SupportedModels[s.model]
	if !ok {
		mInfo = SupportedModels["gemini-2.0-flash"]
	}

	u := uuid.New().String()
	uUpper := strings.ToUpper(u)

	// Build conversation context (canonical 69-element structure)
	inner := make([]interface{}, 69)
	inner[0] = []interface{}{message, 0, nil, nil, nil, nil, 0}
	inner[1] = []interface{}{"en"}
	inner[2] = s.buildMetadata()
	inner[6] = []interface{}{1}
	inner[7] = 1 // streaming
	inner[10] = 1
	inner[11] = 0
	inner[17] = []interface{}{[]interface{}{0}}
	inner[18] = 0
	inner[27] = 1
	inner[30] = []interface{}{4}
	inner[41] = []interface{}{1}
	inner[53] = 0
	inner[59] = uUpper // UUID must match x-goog-ext-525005358-jspb header
	inner[61] = []interface{}{}
	inner[68] = 2

	innerJSON := marshalNoEscape(inner)
	outer := []interface{}{nil, innerJSON}
	outerJSON := marshalNoEscape(outer)

	formData := map[string]string{
		"at":    at,
		"f.req": outerJSON,
	}

	s.worker.mu.RLock()
	bl := s.worker.buildLabel
	sid := s.worker.sessionID
	s.worker.mu.RUnlock()

	s.worker.muCounter.Lock()
	s.worker.requestCounter += 100000
	reqID := s.worker.requestCounter
	s.worker.muCounter.Unlock()

	// Dynamic headers
	modelHeaderValue := fmt.Sprintf(`[1,null,null,null,"%s",null,null,0,[4],null,null,%d]`, mInfo.RPCID, mInfo.CapacityTail)
	headers := map[string]string{
		"x-goog-ext-525001261-jspb": modelHeaderValue,
		"x-goog-ext-73010989-jspb":  "[0]",
		"x-goog-ext-73010990-jspb":  "[0]",
		"x-goog-ext-525005358-jspb": fmt.Sprintf(`["%s",1]`, uUpper),
	}

	// Send warmup activity before generate (required to avoid 429 rate limiting)
	if err := s.worker.sendBardActivity(ctx); err != nil {
		s.worker.log.Warn("Warmup activity failed (silent rejection risk)", zap.Error(err))
	}

	s.worker.ReqMu.RLock()
	reqBody := s.worker.httpClient.R().
		SetContext(ctx).
		SetHeaders(headers).
		SetFormData(formData).
		SetQueryParam("rt", "c").
		SetQueryParam("hl", "en").
		SetQueryParam("_reqid", fmt.Sprintf("%d", reqID))

	if bl != "" {
		reqBody.SetQueryParam("bl", bl)
	}
	if sid != "" {
		reqBody.SetQueryParam("f.sid", sid)
	}

	resp, err := reqBody.Post(EndpointGenerate)
	s.worker.ReqMu.RUnlock()

	if err != nil {
		if s.worker.OnError != nil {
			s.worker.OnError(s.worker.AccountID, err)
		}
		return nil, err
	}

	if resp.StatusCode != 200 {
		body := resp.String()
		body500 := body
		if len(body500) > 500 {
			body500 = body500[:500]
		}
		
		isHTML := strings.Contains(body, "<!DOCTYPE html") || strings.Contains(body, "<html>")
		if resp.StatusCode == 401 || resp.StatusCode == 403 {
			err = ErrAccessDenied
		} else if resp.StatusCode == 429 && isHTML {
			err = ErrBotBlocked
		} else {
			err = fmt.Errorf("chat failed with status: %d", resp.StatusCode)
		}
		
		s.worker.log.Error("Chat request error",
			zap.Int("status", resp.StatusCode),
			zap.String("model", s.model),
			zap.String("response_body", body500),
		)
		if s.worker.OnError != nil {
			s.worker.OnError(s.worker.AccountID, err)
		}
		return nil, err
	}

	rawBody := resp.String()
	s.lastRawResponse = rawBody

	response, err := s.worker.parseResponse(rawBody)
	if err != nil {
		if s.worker.OnError != nil {
			s.worker.OnError(s.worker.AccountID, err)
		}
		return nil, err
	}

	if s.worker.OnSuccess != nil {
		s.worker.OnSuccess(s.worker.AccountID)
	}

	// Update session metadata
	if response.Metadata != nil {
		if cid, ok := response.Metadata["cid"].(string); ok && cid != "" {
			if s.metadata == nil {
				s.metadata = &SessionMetadata{}
			}
			s.metadata.ConversationID = cid
		}
		if rid, ok := response.Metadata["rid"].(string); ok && rid != "" {
			if s.metadata == nil {
				s.metadata = &SessionMetadata{}
			}
			s.metadata.ResponseID = rid
		}
		if rcid, ok := response.Metadata["rcid"].(string); ok && rcid != "" {
			if s.metadata == nil {
				s.metadata = &SessionMetadata{}
			}
			s.metadata.ChoiceID = rcid
		}
	}

	// Update history
	s.history = append(s.history, Message{
		Role:    "user",
		Content: message,
	})
	s.history = append(s.history, Message{
		Role:    "model",
		Content: response.Text,
	})

	return response, nil
}

// GetAccountID returns the account ID
func (s *GeminiChatSession) GetAccountID() string {
	if s.worker == nil {
		return ""
	}
	return s.worker.AccountID
}

// GetMetadata returns session metadata
func (s *GeminiChatSession) GetMetadata() *SessionMetadata {
	if s.metadata == nil {
		return &SessionMetadata{
			Model: s.model,
		}
	}
	s.metadata.Model = s.model
	return s.metadata
}

// GetHistory returns conversation history
func (s *GeminiChatSession) GetHistory() []Message {
	return s.history
}

// Clear clears the conversation history
func (s *GeminiChatSession) Clear() {
	s.history = []Message{}
	s.metadata = nil
}

// buildMetadata builds metadata array for API request
func (s *GeminiChatSession) buildMetadata() []interface{} {
	if s.metadata == nil {
		return []interface{}{"", "", "", nil, nil, nil, nil, nil, nil, ""}
	}

	// Full metadata structure: ["cid", "rid", "rcid", null, null, null, null, null, null, ""]
	return []interface{}{
		s.metadata.ConversationID,
		s.metadata.ResponseID,
		s.metadata.ChoiceID,
		nil, nil, nil, nil, nil, nil, "",
	}
}

// Delete deletes the chat session directly from Gemini Web history via batchexecute.
// Strategy:
//  1. Wait 2 seconds to reduce anti-bot detection risk.
//  2. Try the cached DeleteConfig (RPCID + payload template) from file.
//  3. If that fails (non-200 or Google returns an error), fetch the Gemini app HTML and
//     ask the Oracle (Gemini Pro API key, randomly rotated) for the current RPCID/template.
//  4. Retry the delete with the new config.
//  5. IF and ONLY IF the deletion succeeds, save the (possibly updated) config to disk.
func (s *GeminiChatSession) Delete() error {
	if s.metadata == nil || s.metadata.ConversationID == "" {
		s.worker.log.Debug("🗑️ Delete: skipped — no conversation ID in metadata")
		return nil
	}

	s.worker.mu.RLock()
	at := s.worker.at
	bl := s.worker.buildLabel
	sid := s.worker.sessionID
	s.worker.mu.RUnlock()

	if at == "" {
		return fmt.Errorf("delete: worker not initialized (no 'at' token)")
	}

	// Skip deletion for guest accounts to save resources
	if strings.HasPrefix(s.worker.AccountID, "guest-") {
		s.worker.log.Debug("🗑️ Delete: skipped — guest account does not need chat deletion")
		return nil
	}

	cid := s.metadata.ConversationID
	log := s.worker.log

	log.Info("⏳ Delete: Waiting 2s before deleting to avoid anti-bot detection...",
		zap.String("conversation_id", cid),
		zap.String("account_id", s.worker.AccountID),
	)
	time.Sleep(2 * time.Second)

	// Helper: build and send the batchexecute delete request
	doDelete := func(cfg DeleteConfig) error {
		innerPayload := fmt.Sprintf(cfg.PayloadTemplate, cid)
		reqArray := []interface{}{
			[]interface{}{
				[]interface{}{cfg.RPCID, innerPayload, nil, "generic"},
			},
		}
		reqJSON, _ := json.Marshal(reqArray)

		s.worker.muCounter.Lock()
		s.worker.requestCounter += 100000
		reqID := s.worker.requestCounter
		s.worker.muCounter.Unlock()

		params := map[string]string{
			"rpcids":      cfg.RPCID,
			"source-path": "/app",
			"hl":          "en",
			"_reqid":      fmt.Sprintf("%d", reqID),
			"rt":          "c",
		}
		if bl != "" {
			params["bl"] = bl
		}
		if sid != "" {
			params["f.sid"] = sid
		}

		s.worker.ReqMu.RLock()
		reqJSONStr := string(reqJSON)
		
		// Log raw request for diagnostic purposes
		log.Debug("🗑️ Delete: Sending raw request", 
			zap.String("rpcids", cfg.RPCID),
			zap.String("at", at),
			zap.String("f.req", reqJSONStr),
		)

		resp, err := s.worker.httpClient.R().
			SetContext(context.Background()).
			SetHeaders(map[string]string{
				"Sec-Fetch-Dest": "empty",
				"Sec-Fetch-Mode": "cors",
				"Sec-Fetch-Site": "same-origin",
			}).
			SetQueryParams(params).
			SetFormData(map[string]string{
				"at":    at,
				"f.req": reqJSONStr,
			}).
			Post(EndpointBatchExec)
		s.worker.ReqMu.RUnlock()

		if err != nil {
			return fmt.Errorf("delete request failed: %w", err)
		}

		// Google batchexecute returns HTTP 200 for successful deletes.
		if resp.StatusCode == 200 {
			return nil
		}

		// Log raw response on failure
		log.Warn("🗑️ Delete: Request FAILED",
			zap.Int("status", resp.StatusCode),
			zap.String("rpcid", cfg.RPCID),
			zap.String("raw_response", resp.String()),
		)

		return fmt.Errorf("delete batchexecute returned status %d", resp.StatusCode)
	}

	// Step 1: Try with cached config
	var activeCfg DeleteConfig
	if s.worker.Client != nil && s.worker.Client.GetDeleteConfigMgr() != nil {
		activeCfg = s.worker.Client.GetDeleteConfigMgr().GetConfig()
		log.Info("🗑️ Delete: Using cached delete config",
			zap.String("rpcid", activeCfg.RPCID),
			zap.String("payload_template", activeCfg.PayloadTemplate),
		)
	} else {
		activeCfg = *DefaultDeleteConfig()
		log.Info("🗑️ Delete: No DeleteConfigManager found, using built-in default config",
			zap.String("rpcid", activeCfg.RPCID),
		)
	}

	err := doDelete(activeCfg)
	if err == nil {
		log.Info("🎉 Delete: SUCCESSFUL (HTTP 200)", zap.String("conversation_id", cid))
		return nil
	}

	log.Warn("🗑️ Delete: Cached config failed — trying priority list before Oracle",
		zap.Error(err),
		zap.String("failed_rpcid", activeCfg.RPCID),
	)

	// Step 1.5: Try priority RPCIDs suggested by user
	priorityConfigs := []DeleteConfig{
		{RPCID: "GzXR5e", PayloadTemplate: `["%s"]`},
		{RPCID: "MaZiqc", PayloadTemplate: `["%s"]`},
		{RPCID: "qWymEb", PayloadTemplate: `["%s",[1,null,0,1]]`},
		{RPCID: "TmdDAd", PayloadTemplate: `["%s"]`},
	}

	excludeRPCIDs := []string{activeCfg.RPCID}
	
	for _, pCfg := range priorityConfigs {
		// Skip if we already tried it via cache
		if pCfg.RPCID == activeCfg.RPCID {
			continue
		}

		log.Info("🗑️ Delete: Trying priority config", zap.String("rpcid", pCfg.RPCID))
		err = doDelete(pCfg)
		if err == nil {
			log.Info("🎉 Delete: SUCCESSFUL with priority config", zap.String("rpcid", pCfg.RPCID))
			// Save to disk
			if s.worker.Client != nil && s.worker.Client.GetDeleteConfigMgr() != nil {
				s.worker.Client.GetDeleteConfigMgr().UpdateConfig(&pCfg)
			}
			return nil
		}
		excludeRPCIDs = append(excludeRPCIDs, pCfg.RPCID)
	}

	// Step 2: Oracle fallback loop — with global discovery lock to prevent redundant API calls
	if s.worker.Client == nil {
		log.Error("🗑️ Delete: No Client reference on worker — cannot invoke Oracle")
		return fmt.Errorf("delete failed and Oracle unavailable (no client reference): %w", err)
	}

	if !s.worker.Client.TryStartDeleteDiscovery() {
		log.Warn("🗑️ Delete: Skip Oracle discovery — another session is already discovering a new config")
		return nil
	}
	defer s.worker.Client.EndDeleteDiscovery()

	s.worker.ReqMu.RLock()
	htmlResp, htmlErr := s.worker.httpClient.R().
		SetContext(context.Background()).Get(EndpointInit)
	s.worker.ReqMu.RUnlock()

	pageHTML := ""
	if htmlErr == nil {
		pageHTML = htmlResp.String()
	}
	
	oracleKeys := s.worker.Client.GetOracleAPIKeys()
	maxAttempts := len(oracleKeys)
	if maxAttempts == 0 {
		return fmt.Errorf("no Oracle API keys available for retry loop")
	}

	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		log.Info(fmt.Sprintf("🗑️ Delete: Invoking DeleteOracle (Attempt %d/%d)...", attempt, maxAttempts))
		oracleCtx, oracleCancel := context.WithTimeout(context.Background(), 60*time.Second)
		
		newCfg, oracleErr := s.worker.Client.callDeleteOracle(oracleCtx, pageHTML, s.lastRawResponse, excludeRPCIDs)
		oracleCancel()

		if oracleErr != nil {
			log.Error("🗑️ Delete: Oracle failed to find new delete config", zap.Error(oracleErr))
			lastErr = oracleErr
			// Oracle fully failed (maybe all keys blocked), break loop
			break
		}

		log.Info("🗑️ Delete: Oracle returned new config — retrying delete",
			zap.String("new_rpcid", newCfg.RPCID),
			zap.String("new_payload_template", newCfg.PayloadTemplate),
		)

		err = doDelete(*newCfg)
		if err == nil {
			log.Info("🎉 Delete: SUCCESSFUL with Oracle new config",
				zap.String("conversation_id", cid),
				zap.String("rpcid", newCfg.RPCID),
			)
			// Save to disk
			if s.worker.Client.GetDeleteConfigMgr() != nil {
				s.worker.Client.GetDeleteConfigMgr().UpdateConfig(newCfg)
				log.Info("💾 Delete: New delete config SAVED to disk")
			}
			return nil
		}

		log.Warn("🗑️ Delete: Config from Oracle FAILED during attempt",
			zap.String("failed_rpcid", newCfg.RPCID),
			zap.Error(err),
		)
		excludeRPCIDs = append(excludeRPCIDs, newCfg.RPCID)
		lastErr = err
	}

	log.Error("🗑️ Delete: Completely failed to delete after all Oracle attempts", zap.Error(lastErr))
	return fmt.Errorf("delete failed completely: %w", lastErr)
}
