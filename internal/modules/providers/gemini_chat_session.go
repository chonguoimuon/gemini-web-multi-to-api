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
	worker   *Worker // Refers to providers.Worker
	model    string
	metadata *SessionMetadata
	history  []Message
}

// SendMessage sends a message in the chat session
func (s *GeminiChatSession) SendMessage(ctx context.Context, message string, options ...GenerateOption) (*Response, error) {
	// Read session token safely — short critical section, no lock held during HTTP call
	s.worker.mu.RLock()
	at := s.worker.at
	s.worker.mu.RUnlock()

	if at == "" {
		return nil, fmt.Errorf("client not initialized")
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
	s.worker.sendBardActivity(ctx)

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
		if resp.StatusCode == 401 || resp.StatusCode == 403 {
			err = ErrAccessDenied
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

	response, err := s.worker.parseResponse(resp.String())
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

// Delete deletes the chat session from Gemini Web history
func (s *GeminiChatSession) Delete() error {
	if s.metadata == nil || s.metadata.ConversationID == "" {
		return nil
	}

	s.worker.mu.RLock()
	at := s.worker.at
	s.worker.mu.RUnlock()

	if at == "" {
		return fmt.Errorf("client not initialized")
	}

	cid := s.metadata.ConversationID
	if cid == "" {
		return nil
	}

	s.worker.log.Info("🔄 Attempting to delete chat via MyActivity API...")

	// 1. Fetch the 'at' token for MyActivity endpoint
	respHTML, err := s.worker.httpClient.R().
		SetContext(context.Background()).
		Get("https://myactivity.google.com/product/gemini")

	if err != nil {
		return fmt.Errorf("failed to fetch myactivity page: %v", err)
	}

	myActivityBody := respHTML.String()

	// Extract the SNlM0e token (at token) for myactivity
	var myActivityAT string
	if start := strings.Index(myActivityBody, `SNlM0e":"`); start != -1 {
		end := strings.Index(myActivityBody[start+9:], `"`)
		if end != -1 {
			myActivityAT = myActivityBody[start+9 : start+9+end]
		}
	}

	if myActivityAT == "" {
		return fmt.Errorf("could not find 'at' token for myactivity")
	}

	// 2. Build the TmdDAd payload (delete recent activity)
	// Delete activity from 1 hour ago to 1 hour in the future
	now := time.Now().Unix()
	startTime := now - 3600*24 // past 24 hours to be safe
	endTime := now + 3600

	innerPayload := fmt.Sprintf(`[[null,null,null,null,null,null,null,["bard"]],null,[[[%d],[%d,999999999]]]]`, startTime, endTime)

	reqArray := []interface{}{
		[]interface{}{
			[]interface{}{"TmdDAd", innerPayload, nil, "generic"},
		},
	}

	reqJSON, _ := json.Marshal(reqArray)

	formData := map[string]string{
		"at":    myActivityAT,
		"f.req": string(reqJSON),
	}

	resp, err := s.worker.httpClient.R().
		SetContext(context.Background()).
		SetFormData(formData).
		SetQueryParam("rpcids", "TmdDAd").
		SetQueryParam("source-path", "/product/gemini").
		SetQueryParam("rt", "c").
		SetQueryParam("at", myActivityAT).
		Post("https://myactivity.google.com/_/FootprintsMyactivityUi/data/batchexecute")

	if err != nil {
		return fmt.Errorf("myactivity batchexecute failed: %v", err)
	}

	if resp.StatusCode == 200 && !strings.Contains(resp.String(), "error") && !strings.Contains(resp.String(), "Error") {
		s.worker.log.Info("🎉 Delete Chat SUCCESSFUL via MyActivity TmdDAd payload!")
		return nil
	}

	s.worker.log.Debug("Failed MyActivity payload", zap.Int("status", resp.StatusCode), zap.String("body", resp.String()))
	return fmt.Errorf("failed to delete via myactivity, status: %d", resp.StatusCode)
}
