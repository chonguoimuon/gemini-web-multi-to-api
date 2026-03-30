package providers

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"gemini-web-to-api/internal/commons/configs"

	"github.com/imroc/req/v3"
	"go.uber.org/zap"
)

var (
	ErrAccessDenied = errors.New("access denied: account may be blocked or unauthenticated")
)

type Worker struct {
	AccountID  string
	httpClient *req.Client
	cookies    *CookieStore
	at         string
	mu         sync.RWMutex // protects: at, healthy
	ReqMu      sync.RWMutex // explicitly protects active HTTP client sessions vs cookie refreshes
	healthy    bool
	log        *zap.Logger

	autoRefresh     bool
	refreshInterval time.Duration
	stopRefresh     chan struct{}
	maxRetries      int
	cachedModels    []ModelInfo
	allCookies      map[string]*http.Cookie // managed cookie map to prevent SetCommonCookies accumulation

	isBusy bool
	busyMu sync.Mutex

	OnSuccess func(accountID string)
	OnError   func(accountID string, err error)
}

type CookieStore struct {
	Secure1PSID   string    `json:"__Secure-1PSID"`
	Secure1PSIDTS string    `json:"__Secure-1PSIDTS"`
	UpdatedAt     time.Time `json:"updated_at"`
	mu            sync.RWMutex
}

const (
	defaultRefreshIntervalMinutes = 30
)

func NewWorker(cfg *configs.Config, log *zap.Logger, psid, psidTS string) *Worker {
	cookies := &CookieStore{
		Secure1PSID:   psid,
		Secure1PSIDTS: psidTS,
		UpdatedAt:     time.Now(),
	}

	client := req.NewClient().
		SetTimeout(10 * time.Minute).
		SetCommonHeaders(DefaultHeaders)

	refreshIntervalMinutes := cfg.Gemini.RefreshInterval
	if refreshIntervalMinutes <= 0 {
		refreshIntervalMinutes = defaultRefreshIntervalMinutes
	}

	return &Worker{
		httpClient:      client,
		cookies:         cookies,
		autoRefresh:     true,
		refreshInterval: time.Duration(refreshIntervalMinutes) * time.Minute,
		stopRefresh:     make(chan struct{}),
		maxRetries:      cfg.Gemini.MaxRetries,
		log:             log,
		allCookies:      make(map[string]*http.Cookie),
	}
}

func (c *Worker) Init(ctx context.Context) error {
	// Clean cookies
	c.cookies.Secure1PSID = cleanCookie(c.cookies.Secure1PSID)
	configPSIDTS := cleanCookie(c.cookies.Secure1PSIDTS) // Save original config value
	c.cookies.Secure1PSIDTS = configPSIDTS

	// Check if we should use cached cookies or clear cache
	if c.cookies.Secure1PSID != "" {
		cachedTS, err := c.LoadCachedCookies()
		
		// If config has a new PSIDTS that differs from cache, clear cache and use config
		if configPSIDTS != "" && cachedTS != "" && configPSIDTS != cachedTS {
			_ = c.ClearCookieCache()
			// Keep using the config value (already set above)
		} else if err == nil && cachedTS != "" && configPSIDTS == "" {
			// Only use cache if config doesn't provide PSIDTS
			c.cookies.Secure1PSIDTS = cachedTS
			c.log.Info("Loaded __Secure-1PSIDTS from cache")
		}
	}

	// Obtain PSIDTS via rotation if missing
	if c.cookies.Secure1PSID != "" && c.cookies.Secure1PSIDTS == "" {
		c.log.Info("Only __Secure-1PSID provided, attempting to obtain __Secure-1PSIDTS via rotation...")
		if err := c.RotateCookies(); err != nil {
			c.log.Info("Rotation failed, proceeding with just __Secure-1PSID (might fail)", zap.String("error", err.Error()))
		} else {
			c.log.Info("Successfully obtained __Secure-1PSIDTS via rotation")
		}
	}

	// Populate cookies using deduplicated map
	c.updateCookies(nil)

	// Get SNlM0e token
	err := c.refreshSessionToken()
	if err != nil {
		c.log.Debug("Initial session token fetch failed, attempting cookie rotation", zap.Error(err))
		// Try to rotate cookies and retry
		if rotErr := c.RotateCookies(); rotErr == nil {
			c.log.Debug("Cookie rotation succeeded, retrying session token fetch")
			err = c.refreshSessionToken()
		} else {
			c.log.Debug("Cookie rotation failed", zap.Error(rotErr))
		}
	}

	if err != nil {
		return err
	}

	// Save the valid cookies to cache immediately after successful init
	_ = c.SaveCachedCookies()

	c.log.Info("✅ Gemini client initialized successfully")

	// 5. Start auto-refresh in background
	if c.autoRefresh {
		go c.startAutoRefresh()
	}

	return nil
}

func (c *Worker) TestConnection() error {
	err := c.refreshSessionToken()
	if err != nil {
		c.log.Debug("TestConnection: session token fetch failed, attempting cookie rotation", zap.Error(err))
		if rotErr := c.RotateCookies(); rotErr == nil {
			err = c.refreshSessionToken()
		}
	}

	c.mu.Lock()
	c.healthy = (err == nil)
	c.mu.Unlock()

	return err
}

func (c *Worker) refreshSessionToken() error {
	// 1. Initial hit to google.com to get extra cookies (NID, etc)
	tmpClient := req.NewClient().
		SetTimeout(30 * time.Second).
		SetUserAgent("Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")
	
	resp1, err := tmpClient.R().Get("https://www.google.com/")
	extraCookies := ""
	if err == nil {
		parts := []string{}
		for _, ck := range resp1.Cookies() {
			parts = append(parts, fmt.Sprintf("%s=%s", ck.Name, ck.Value))
		}
		// Sync to main client with deduplication
		c.updateCookies(resp1.Cookies())
		if len(parts) > 0 {
			extraCookies = strings.Join(parts, "; ") + "; "
		}
	}

	// 2. Prepare full cookie string
	cookieStr := fmt.Sprintf("%s__Secure-1PSID=%s; __Secure-1PSIDTS=%s", 
		extraCookies, c.cookies.Secure1PSID, c.cookies.Secure1PSIDTS)

	commonHeaders := map[string]string{
		"Accept":                    "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8,application/signed-exchange;v=b3;q=0.7",
		"Accept-Language":           "en-US,en;q=0.9",
		"Cache-Control":             "max-age=0",
		"Origin":                    "https://gemini.google.com",
		"Sec-Ch-Ua":                 `"Not_A Brand";v="8", "Chromium";v="120", "Google Chrome";v="120"`,
		"Sec-Ch-Ua-Mobile":          "?0",
		"Sec-Ch-Ua-Platform":        `"Windows"`,
		"Sec-Fetch-Dest":            "document",
		"Sec-Fetch-Mode":            "navigate",
		"Sec-Fetch-Site":            "none",
		"Sec-Fetch-User":            "?1",
		"Upgrade-Insecure-Requests": "1",
		"X-Same-Domain":             "1",
		"User-Agent":                "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36",
	}

	hClient := &http.Client{
		Timeout: 30 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return nil // follow redirects
		},
	}

	// Helper to merge cookies into a map to avoid duplicates
	mergeCookies := func(baseStr string, newCks []*http.Cookie) string {
		m := make(map[string]string)
		for _, part := range strings.Split(baseStr, ";") {
			p := strings.TrimSpace(part)
			if p == "" {
				continue
			}
			kv := strings.SplitN(p, "=", 2)
			if len(kv) == 2 {
				m[kv[0]] = kv[1]
			}
		}
		for _, ck := range newCks {
			m[ck.Name] = ck.Value
		}
		res := []string{}
		for k, v := range m {
			res = append(res, fmt.Sprintf("%s=%s", k, v))
		}
		return strings.Join(res, "; ")
	}

	req1, _ := http.NewRequest("GET", "https://gemini.google.com/?hl=en", nil)
	for k, v := range commonHeaders {
		req1.Header.Set(k, v)
	}
	req1.Header.Set("Cookie", cookieStr)
	resp1_direct, _ := hClient.Do(req1)
	if resp1_direct != nil {
		cookieStr = mergeCookies(cookieStr, resp1_direct.Cookies())
		c.updateCookies(resp1_direct.Cookies())
		resp1_direct.Body.Close()
	}

	// 2. The main INIT hit
	req2, _ := http.NewRequest("GET", EndpointInit+"?hl=en", nil)
	for k, v := range commonHeaders {
		req2.Header.Set(k, v)
	}
	req2.Header.Set("Sec-Fetch-Site", "same-origin")
	req2.Header.Set("Cookie", cookieStr)
	req2.Header.Set("Referer", "https://gemini.google.com/")
	req2.Header.Set("Accept-Encoding", "gzip, deflate, br")

	resp, err := hClient.Do(req2)
	if err != nil {
		return fmt.Errorf("failed to reach gemini app: %w", err)
	}
	defer resp.Body.Close()

	// Dump for debugging if it fails
	// reqDump, _ := httputil.DumpRequestOut(req2, false)
	// respDump, _ := httputil.DumpResponse(resp, false)
	
	var bodyReader io.ReadCloser = resp.Body
	if strings.Contains(resp.Header.Get("Content-Encoding"), "gzip") {
		gz, err := gzip.NewReader(resp.Body)
		if err == nil {
			bodyReader = gz
			defer gz.Close()
		}
	}

	bodyBytes, _ := io.ReadAll(bodyReader)
	body := string(bodyBytes)


	re := regexp.MustCompile(`"SNlM0e":"([^"]+)"`)
	matches := re.FindStringSubmatch(body)
	if len(matches) < 2 {
		reFallback := regexp.MustCompile(`\["SNlM0e","([^"]+)"\]`)
		matches = reFallback.FindStringSubmatch(body)
		if len(matches) < 2 {


			errMsg := "authentication failed: SNlM0e not found"
			if strings.Contains(body, "Sign in") || strings.Contains(body, "login") {
				errMsg = "authentication failed: cookies invalid. Please provide __Secure-1PSIDTS in addition to __Secure-1PSID"
			}

			// Log as Info to avoid stack trace for expected auth failures
			c.log.Info(errMsg)
			return fmt.Errorf("%s", errMsg)
		}
	}

	c.mu.Lock()
	c.at = matches[1]
	c.healthy = true
	c.mu.Unlock()

	// Update dynamic models from the same initialization body
	c.refreshModels(body)

	return nil
}

func (c *Worker) refreshModels(body string) {
	var newModels []ModelInfo
	now := time.Now().Unix()

	// Improved regex to find gemini model IDs even when escaped in JSON
	// Matches IDs like gemini-2.0-flash, gemini-1.5-pro, etc.
	// We look for gemini- followed by alphanumeric characters, dots, or dashes.
	modelIDRegex := regexp.MustCompile(`gemini-[a-zA-Z0-9.-]+`)
	matches := modelIDRegex.FindAllString(body, -1)
	
	uniqueIDs := make(map[string]bool)
	for _, id := range matches {
		// Clean up potential trailing backslashes or quotes if they were caught
		id = strings.Trim(id, `\"' `)
		
		// Basic validation: ensure it doesn't look like a generic string or partial ID
		if !uniqueIDs[id] && len(id) > 10 {
			uniqueIDs[id] = true
			newModels = append(newModels, ModelInfo{
				ID:       id,
				Created:  now,
				OwnedBy:  "google",
				Provider: "gemini",
			})
		}
	}

	c.mu.Lock()
	c.cachedModels = newModels
	c.mu.Unlock()
	
	if len(newModels) == 0 {
		c.log.Warn("⚠️ No models found in Gemini Web response. Please check your cookies or connection.")
	} else {
		ids := make([]string, 0, len(newModels))
		for _, m := range newModels {
			ids = append(ids, m.ID)
		}
		c.log.Info("🔄 Refreshed available models from Gemini Web", zap.Int("count", len(newModels)), zap.Strings("models", ids))
	}
}

// startAutoRefresh periodically refreshes the PSIDTS cookie
func (c *Worker) startAutoRefresh() {
	ticker := time.NewTicker(c.refreshInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			c.log.Debug("Starting scheduled cookie refresh")
			
			c.ReqMu.Lock()
			rotateErr := c.RotateCookies()
			c.ReqMu.Unlock()
			
			if rotateErr != nil {
				// Do NOT assume 401/403 from RotateCookies means the account is permanently banned or expired, 
				// because Google's RotateCookies endpoint is highly aggressive against automated requests.
				// Fallback: try to refresh the session token (SNlM0e/at) to keep client alive
				c.log.Warn("Cookie rotation failed, falling back to session token refresh", zap.Error(rotateErr))
				
				c.ReqMu.Lock()
				sessionErr := c.refreshSessionToken()
				c.ReqMu.Unlock()
				
				if sessionErr != nil {
					// Both methods failed - mark client as unhealthy so callers know
					c.log.Error("Session token refresh also failed, marking client unhealthy",
						zap.NamedError("rotation_error", rotateErr),
						zap.NamedError("session_error", sessionErr),
					)
					c.mu.Lock()
					c.healthy = false
					c.mu.Unlock()

					if c.OnError != nil {
						c.OnError(c.AccountID, fmt.Errorf("background refresh failed: rotation=%v, session=%v", rotateErr, sessionErr))
					}
				} else {
					c.log.Info("Session token refreshed successfully after rotation failure")
					// Ensure client is marked healthy since session token is valid
					c.mu.Lock()
					c.healthy = true
					c.mu.Unlock()

					if c.OnSuccess != nil {
						c.OnSuccess(c.AccountID)
					}
				}
			} else {
				// Rotation succeeded - also refresh session token to keep SNlM0e/at up to date
				c.ReqMu.Lock()
				sessionErr := c.refreshSessionToken()
				c.ReqMu.Unlock()
				
				if sessionErr != nil {
					c.log.Warn("Cookie rotated but session token refresh failed", zap.Error(sessionErr))
					// Still mark as healthy since rotation worked
					c.mu.Lock()
					c.healthy = true
					c.mu.Unlock()
					if c.OnSuccess != nil {
						c.OnSuccess(c.AccountID)
					}
				} else {
					c.log.Info("Cookie and session token refreshed successfully")
					c.mu.Lock()
					c.healthy = true
					c.mu.Unlock()
					if c.OnSuccess != nil {
						c.OnSuccess(c.AccountID)
					}
				}
			}
		case <-c.stopRefresh:
			return
		}
	}
}

func (c *Worker) RotateCookies() error {
	// 1. Gather all cookies safely without holding locks during the HTTP call.
	// Lock order: always c.mu THEN c.cookies.mu to prevent deadlocks.
	var cookieParts []string
	
	c.mu.RLock()
	for _, ck := range c.allCookies {
		if ck.Name != "__Secure-1PSID" && ck.Name != "__Secure-1PSIDTS" {
			cookieParts = append(cookieParts, fmt.Sprintf("%s=%s", ck.Name, ck.Value))
		}
	}
	c.mu.RUnlock()

	c.cookies.mu.RLock()
	if c.cookies.Secure1PSID != "" {
		cookieParts = append(cookieParts, fmt.Sprintf("__Secure-1PSID=%s", c.cookies.Secure1PSID))
	}
	if c.cookies.Secure1PSIDTS != "" {
		cookieParts = append(cookieParts, fmt.Sprintf("__Secure-1PSIDTS=%s", c.cookies.Secure1PSIDTS))
	}
	c.cookies.mu.RUnlock()

	cookieStr := strings.Join(cookieParts, "; ")

	// Payload must be exactly this string
	strBody := `[000,"-0000000000000000000"]`
	req, _ := http.NewRequest("POST", EndpointRotateCookies, strings.NewReader(strBody))
	
	req.Header.Set("Content-Type", "application/json")
	// Google often blocks requests with default Go-http-client User-Agent
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")
	req.Header.Set("Cookie", cookieStr)

	c.log.Debug("Sending rotation request", zap.String("url", EndpointRotateCookies))
	hClient := &http.Client{Timeout: 5 * time.Second}
	resp, err := hClient.Do(req)
	if err != nil {
		c.log.Info("Rotation request failed (network/auth issue)", zap.String("error", err.Error()))
		return fmt.Errorf("failed to call rotation endpoint: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		c.log.Info("Rotation failed (likely invalid __Secure-1PSID)", zap.Int("status", resp.StatusCode))
		return fmt.Errorf("rotation failed with status %d", resp.StatusCode)
	}

	// Extract new PSIDTS from Set-Cookie headers
	found := false
	var newCookies []*http.Cookie
	
	c.cookies.mu.Lock()
	for _, cookie := range resp.Cookies() {
		if cookie.Name == "__Secure-1PSIDTS" {
			c.cookies.Secure1PSIDTS = cookie.Value
			c.cookies.UpdatedAt = time.Now()
			found = true
			// Save the new cookie to cache immediately
			_ = c.SaveCachedCookies()
		}
		newCookies = append(newCookies, cookie)
	}
	c.cookies.mu.Unlock()

	// Sync to req/v3 client for future calls
	if len(newCookies) > 0 {
		c.updateCookies(newCookies)
	}

	if found {
		c.log.Info("Cookie rotated successfully", zap.Time("updated_at", time.Now()))
		return nil
	}

	return errors.New("no new __Secure-1PSIDTS cookie received")
}

func (c *Worker) GetCookies() *CookieStore {
	c.cookies.mu.RLock()
	defer c.cookies.mu.RUnlock()
	
	return &CookieStore{
		Secure1PSID:   c.cookies.Secure1PSID,
		Secure1PSIDTS: c.cookies.Secure1PSIDTS,
		UpdatedAt:     c.cookies.UpdatedAt,
	}
}

func (c *Worker) GenerateContent(ctx context.Context, prompt string, options ...GenerateOption) (*Response, error) {
	config := &GenerateConfig{}
	for _, opt := range options {
		opt(config)
	}

	// Default to first available model if not set or "gemini-pro"
	c.mu.RLock()
	if config.Model == "" || config.Model == "gemini-pro" {
		if len(c.cachedModels) > 0 {
			config.Model = c.cachedModels[0].ID
		}
	}
	
	// Strictly enforce that we only use models found/confirmed from the web
	found := false
	for _, m := range c.cachedModels {
		if m.ID == config.Model {
			found = true
			break
		}
	}
	at := c.at
	c.mu.RUnlock()

	if !found && config.Model != "" {
		return nil, fmt.Errorf("model '%s' is not supported or not available. Available models: %v", config.Model, c.ListModelsIDs())
	}

	if at == "" {
		return nil, errors.New("client not initialized")
	}

	// Build request payload
	// The structure confirmed to work for model selection is [ [prompt], nil, nil, model ]
	inner := []interface{}{
		[]interface{}{prompt},
		nil,
		nil,
		config.Model,
	}

	innerJSON, _ := json.Marshal(inner)
	outer := []interface{}{nil, string(innerJSON)}
	outerJSON, _ := json.Marshal(outer)

	formData := map[string]string{
		"at":    at,
		"f.req": string(outerJSON),
	}

	maxAttempts := c.maxRetries
	if maxAttempts <= 0 {
		maxAttempts = 1
	}

	totalStart := time.Now()
	httpStart := time.Now()
	
	c.SetBusy(true)
	defer c.SetBusy(false)

	c.ReqMu.RLock()
	resp, err := c.httpClient.R().
		SetContext(ctx).
		SetFormData(formData).
		SetQueryParam("at", at).
		Post(EndpointGenerate)
	c.ReqMu.RUnlock()

	httpDuration := time.Since(httpStart)
	if err != nil {
		c.log.Error("Generate request failed",
			zap.Error(err),
			zap.Duration("http_duration", httpDuration),
		)
		if c.OnError != nil {
			c.OnError(c.AccountID, err)
		}
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		err = fmt.Errorf("generate failed with status: %d", resp.StatusCode)
		c.log.Error("Server returned error status",
			zap.Int("status", resp.StatusCode),
		)
		if c.OnError != nil {
			c.OnError(c.AccountID, err)
		}
		return nil, err
	}

	parseStart := time.Now()
	result, parseErr := c.parseResponse(resp.String())
	parseDuration := time.Since(parseStart)

	if parseErr != nil {
		c.log.Error("Failed to parse response",
			zap.Error(parseErr),
		)
		if c.OnError != nil {
			c.OnError(c.AccountID, parseErr)
		}
		return nil, parseErr
	}

	if c.OnSuccess != nil {
		c.OnSuccess(c.AccountID)
	}

	c.log.Debug("GenerateContent timing",
		zap.Duration("gemini_server_rtt", httpDuration),
		zap.Duration("parse_duration", parseDuration),
		zap.Duration("total_duration", time.Since(totalStart)),
		zap.Int("response_bytes", len(resp.String())),
	)

	return result, nil
}

func (c *Worker) StartChat(options ...ChatOption) ChatSession {
	config := &ChatConfig{}
	for _, opt := range options {
		opt(config)
	}

	c.mu.RLock()
	if config.Model == "" || config.Model == "gemini-pro" {
		if len(c.cachedModels) > 0 {
			config.Model = c.cachedModels[0].ID
		}
	}
	c.mu.RUnlock()

	return &GeminiChatSession{
		worker:   c,
		model:    config.Model,
		metadata: config.Metadata,
		history:  []Message{},
	}
}

func (c *Worker) Close() error {
	close(c.stopRefresh)
	c.mu.Lock()
	c.healthy = false
	c.mu.Unlock()
	return nil
}

func (c *Worker) SetBusy(busy bool) {
	c.busyMu.Lock()
	defer c.busyMu.Unlock()
	c.isBusy = busy
}

func (c *Worker) IsBusy() bool {
	c.busyMu.Lock()
	defer c.busyMu.Unlock()
	return c.isBusy
}

func (c *Worker) GetName() string {
	return "gemini"
}

func (c *Worker) IsHealthy() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.healthy
}

func (c *Worker) ListModels() []ModelInfo {
	c.mu.RLock()
	defer c.mu.RUnlock()
	
	if len(c.cachedModels) == 0 {
		return []ModelInfo{}
	}
	
	return c.cachedModels
}

func (c *Worker) ListModelsIDs() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	
	ids := make([]string, 0, len(c.cachedModels))
	for _, m := range c.cachedModels {
		ids = append(ids, m.ID)
	}
	return ids
}

// parseResponse parses Gemini's response format
func (c *Worker) parseResponse(text string) (*Response, error) {
	lines := strings.Split(text, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		line = strings.TrimPrefix(line, ")]}'")

		// Detect specific Google internal errors that indicate access denied
		if strings.Contains(line, "wrb.fr") && strings.Contains(line, "[13]") {
			return nil, ErrAccessDenied
		}

		var root []interface{}
		if err := json.Unmarshal([]byte(line), &root); err == nil {
			for _, item := range root {
				itemArray, ok := item.([]interface{})
				if !ok || len(itemArray) < 3 {
					continue
				}

				payloadStr, ok := itemArray[2].(string)
				if !ok {
					continue
				}

				var payload []interface{}
				if err := json.Unmarshal([]byte(payloadStr), &payload); err != nil {
					continue
				}

				if len(payload) > 4 {
					candidates, ok := payload[4].([]interface{})
					if ok && candidates != nil && len(candidates) > 0 {
						firstCandidate, ok := candidates[0].([]interface{})
						if ok && len(firstCandidate) >= 2 {
							contentParts, ok := firstCandidate[1].([]interface{})
							if ok && len(contentParts) > 0 {
								resText, ok := contentParts[0].(string)
								if ok {
									// Extract conversation metadata if available
									var cid, rid, rcid string
									if len(firstCandidate) > 0 {
										if id, ok := firstCandidate[0].(string); ok {
											rcid = id
										}
									}
									if len(payload) > 1 {
										if id, ok := payload[1].(string); ok {
											cid = id
										} else if idArr, ok := payload[1].([]interface{}); ok && len(idArr) > 0 {
											if idNested, ok := idArr[0].(string); ok {
												cid = idNested
											}
										}
									}
									if len(payload) > 2 {
										if id, ok := payload[2].(string); ok {
											rid = id
										}
									}

									// DEBUG LOG TO SEE PAYLOAD STRUCTURE
									c.log.Debug("Gemini Response Payload Indices",
										zap.Any("payload_1", func() interface{} { 
											if len(payload) > 1 { return payload[1] } 
											return nil 
										}()),
										zap.Any("payload_2", func() interface{} { 
											if len(payload) > 2 { return payload[2] } 
											return nil 
										}()),
										zap.String("full_payload_str", payloadStr),
									)

									// Extract generated image URLs using regex pattern
									pattern := `https://lh3\.googleusercontent\.com/[a-zA-Z0-9\-_]{50,}(?:=[a-zA-Z0-9\-_]+|/fife/|/image/)[a-zA-Z0-9\-_]*`
									re := regexp.MustCompile(pattern)
									matches := re.FindAllString(payloadStr, -1)
									
									var extractedImages []Image
									uniqueMap := make(map[string]bool)
									
									for _, url := range matches {
										if !uniqueMap[url] {
											uniqueMap[url] = true
											extractedImages = append(extractedImages, Image{
												URL: url,
											})
										}
									}

									if len(extractedImages) > 0 {
										c.log.Info("🖼️ Successfully extracted generated images from Gemini", zap.Int("count", len(extractedImages)))
									}

									return &Response{
										Text: resText,
										Images: extractedImages,
										Metadata: map[string]any{
											"cid":  cid,
											"rid":  rid,
											"rcid": rcid,
										},
									}, nil
								}
							}
						}
					}
				}
			}
		}
	}

	sample := text
	if len(sample) > 500 {
		sample = sample[:500]
	}
	return nil, fmt.Errorf("failed to parse response. Sample: %s", sample)
}

func (cs *CookieStore) ToHTTPCookies() []*http.Cookie {
	cs.mu.RLock()
	defer cs.mu.RUnlock()

	cookies := []*http.Cookie{}
	domain := ".google.com"

	if cs.Secure1PSID != "" {
		cookies = append(cookies, &http.Cookie{
			Name:     "__Secure-1PSID",
			Value:    cleanCookie(cs.Secure1PSID),
			Domain:   domain,
			Path:     "/",
			Secure:   true,
			HttpOnly: true,
			SameSite: http.SameSiteNoneMode,
		})
	}
	if cs.Secure1PSIDTS != "" {
		cookies = append(cookies, &http.Cookie{
			Name:     "__Secure-1PSIDTS",
			Value:    cleanCookie(cs.Secure1PSIDTS),
			Domain:   domain,
			Path:     "/",
			Secure:   true,
			HttpOnly: true,
			SameSite: http.SameSiteNoneMode,
		})
	}
	return cookies
}

func cleanCookie(v string) string {
	v = strings.TrimSpace(v)
	v = strings.Trim(v, "\"")
	v = strings.Trim(v, "'")
	v = strings.TrimSuffix(v, ";")
	return v
}

// LoadCachedCookies attempts to read the saved 1PSIDTS from disk
func (c *Worker) LoadCachedCookies() (string, error) {
	if c.cookies.Secure1PSID == "" {
		return "", errors.New("no PSID available")
	}

	hash := sha256.Sum256([]byte(c.cookies.Secure1PSID))
	filename := filepath.Join(".cookies", hex.EncodeToString(hash[:])+".txt")

	data, err := os.ReadFile(filename)
	if err != nil {
		return "", err
	}

	ts := strings.TrimSpace(string(data))
	if ts == "" {
		return "", errors.New("empty cache file")
	}
	return ts, nil
}

// updateCookies maintains a deduplicated map of cookies and sets a single unified Cookie header
func (c *Worker) updateCookies(newCookies []*http.Cookie) {
	c.mu.Lock()
	defer c.mu.Unlock()

	for _, ck := range newCookies {
		c.allCookies[ck.Name] = ck
	}

	// Always ensure our core PSID/PSIDTS override anything else
	c.cookies.mu.RLock()
	if c.cookies.Secure1PSID != "" {
		c.allCookies["__Secure-1PSID"] = &http.Cookie{Name: "__Secure-1PSID", Value: cleanCookie(c.cookies.Secure1PSID)}
	}
	if c.cookies.Secure1PSIDTS != "" {
		c.allCookies["__Secure-1PSIDTS"] = &http.Cookie{Name: "__Secure-1PSIDTS", Value: cleanCookie(c.cookies.Secure1PSIDTS)}
	}
	c.cookies.mu.RUnlock()

	var parts []string
	for _, ck := range c.allCookies {
		parts = append(parts, fmt.Sprintf("%s=%s", ck.Name, ck.Value))
	}
	cookieStr := strings.Join(parts, "; ")

	// Replace all cookies with a single managed header string.
	// This prevents req.Client from infinitely appending cookies to the headers array.
	c.httpClient.SetCommonHeader("Cookie", cookieStr)
}

// SaveCachedCookies writes the current 1PSIDTS to disk
func (c *Worker) SaveCachedCookies() error {
	if c.cookies.Secure1PSID == "" || c.cookies.Secure1PSIDTS == "" {
		return nil
	}

	// Create directory if not exists
	if err := os.MkdirAll(".cookies", 0755); err != nil {
		return err
	}

	hash := sha256.Sum256([]byte(c.cookies.Secure1PSID))
	filename := filepath.Join(".cookies", hex.EncodeToString(hash[:])+".txt")

	err := os.WriteFile(filename, []byte(c.cookies.Secure1PSIDTS), 0600)
	if err == nil {
		c.log.Debug("Saved __Secure-1PSIDTS to local cache for future use", zap.String("file", filename))
	} else {
		c.log.Warn("Failed to save cookies to cache", zap.String("file", filename), zap.Error(err))
	}
	return err
}

// ClearCookieCache deletes the cached cookie file for the current PSID
func (c *Worker) ClearCookieCache() error {
	if c.cookies.Secure1PSID == "" {
		return nil
	}

	hash := sha256.Sum256([]byte(c.cookies.Secure1PSID))
	filename := filepath.Join(".cookies", hex.EncodeToString(hash[:])+".txt")

	err := os.Remove(filename)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	
	return nil
}

const (
EndpointGoogle        = "https://www.google.com"
EndpointInit          = "https://gemini.google.com/app"
EndpointGenerate      = "https://gemini.google.com/_/BardChatUi/data/assistant.lamda.BardFrontendService/StreamGenerate"
EndpointRotateCookies = "https://accounts.google.com/RotateCookies"
EndpointBatchExec     = "https://gemini.google.com/_/BardChatUi/data/batchexecute"
)

var DefaultHeaders = map[string]string{
"Content-Type":  "application/x-www-form-urlencoded;charset=utf-8",
"Origin":        "https://gemini.google.com",
"Referer":       "https://gemini.google.com/",
"User-Agent":    "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36",
"X-Same-Domain": "1",
}
