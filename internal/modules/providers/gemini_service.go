package providers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"gemini-web-to-api/internal/commons/configs"

	"github.com/google/uuid"
	reqv3 "github.com/imroc/req/v3"
	"go.uber.org/zap"
)

var (
	ErrAccessDenied = errors.New("access denied: account may be blocked or unauthenticated")
	ErrSafetyBlock  = errors.New("gemini safety filters are blocking this specific request")
	ErrBotBlocked   = errors.New("gemini blocked this request (anti-bot triggered)")
)

// GeminiModelInfo contains technical data for the StreamGenerate request
type GeminiModelInfo struct {
	RPCID        string
	CapacityTail int
}

// SupportedModels defines the mapping from public IDs (like gemini-2.0-flash) to internal Google hex IDs
var SupportedModels = map[string]GeminiModelInfo{
	"gemini-2.0-flash":          {"fbb127bbb056c959", 1},
	"gemini-2.0-flash-thinking": {"5bf011840784117a", 1},
	"gemini-2.0-pro-exp":        {"9d8ca3786ebdfbea", 1},
	"gemini-1.5-flash":          {"fbb127bbb056c959", 1},
	"gemini-1.5-pro":           {"9d8ca3786ebdfbea", 1},
	"gemini-pro":                {"fbb127bbb056c959", 1}, // Default to Flash for compatibility
}

type Worker struct {
	AccountID  string
	httpClient *reqv3.Client
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
	closed bool // tracks if the refresh loop was stopped

	// New fields for latest RPC structure
	buildLabel     string
	sessionID      string
	requestCounter int
	muCounter      sync.Mutex

	OnSuccess func(accountID string)
	OnError   func(accountID string, err error)
	OnRelease func() // Called when worker transitions from busy→idle, to wake up queue waiters

	SchemaMgr *SchemaManager
	Client    *Client // Back-reference to parent Client for Oracle/Delete config access
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

func NewWorker(cfg *configs.Config, log *zap.Logger, client *Client, psid, psidTS string) *Worker {
	cookies := &CookieStore{
		Secure1PSID:   psid,
		Secure1PSIDTS: psidTS,
		UpdatedAt:     time.Now(),
	}

	httpClient := reqv3.NewClient().
		ImpersonateChrome().
		SetTimeout(10 * time.Minute).
		SetCommonHeaders(DefaultHeaders)

	refreshIntervalMinutes := cfg.Gemini.RefreshInterval
	if refreshIntervalMinutes <= 0 {
		refreshIntervalMinutes = defaultRefreshIntervalMinutes
	}

	// Create a new PRNG for this worker
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))

	return &Worker{
		httpClient:      httpClient,
		cookies:         cookies,
		autoRefresh:     true,
		refreshInterval: time.Duration(refreshIntervalMinutes) * time.Minute,
		stopRefresh:     make(chan struct{}),
		maxRetries:      cfg.Gemini.MaxRetries,
		log:             log,
		allCookies:      make(map[string]*http.Cookie),
		requestCounter:  rng.Intn(9000) + 1000, // Start with 4 digits like Python
		Client:          client,
	}
}

func (c *Worker) SetSchemaManager(sm *SchemaManager) {
	c.SchemaMgr = sm
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
	if err := c.RefreshSession(); err != nil {
		c.log.Debug("Initial session token fetch failed, attempting cookie rotation", zap.Error(err))
		if rotErr := c.RotateCookies(); rotErr == nil {
			c.log.Debug("Cookie rotation succeeded, retrying session token fetch")
			if err := c.RefreshSession(); err != nil {
				return err
			}
		} else {
			return err
		}
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
	err := c.RefreshSession()
	if err != nil {
		c.log.Debug("TestConnection: session token fetch failed, attempting cookie rotation", zap.Error(err))
		if rotErr := c.RotateCookies(); rotErr == nil {
			err = c.RefreshSession()
		}
	}

	c.mu.Lock()
	c.healthy = (err == nil)
	c.mu.Unlock()

	return err
}

// RefreshSession performs a full session initialization with Google
func (c *Worker) RefreshSession() error {
	// 1. Initial hit to google.com to get extra cookies (NID, etc)
	tmpClient := reqv3.NewClient().
		ImpersonateChrome().
		SetTimeout(30 * time.Second).
		SetCookieJar(nil)
	
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
		"Cache-Control":             "max-age=0",
		"Origin":                    "https://gemini.google.com",
		"Sec-Fetch-Dest":            "document",
		"Sec-Fetch-Mode":            "navigate",
		"Sec-Fetch-Site":            "none",
		"Sec-Fetch-User":            "?1",
		"Upgrade-Insecure-Requests": "1",
		"X-Same-Domain":             "1",
	}

	hClient := reqv3.NewClient().
		ImpersonateChrome().
		SetTimeout(30 * time.Second).
		SetCookieJar(nil)

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

	req1 := hClient.R()
	for k, v := range commonHeaders {
		req1.SetHeader(k, v)
	}
	req1.SetHeader("Cookie", cookieStr)
	resp1_direct, _ := req1.Get("https://gemini.google.com/?hl=en")
	if resp1_direct != nil && resp1_direct.IsSuccess() {
		cookieStr = mergeCookies(cookieStr, resp1_direct.Cookies())
		c.updateCookies(resp1_direct.Cookies())
	}

	// 2. The main INIT hit
	req2 := hClient.R()
	for k, v := range commonHeaders {
		req2.SetHeader(k, v)
	}
	req2.SetHeader("Sec-Fetch-Site", "same-origin")
	req2.SetHeader("Cookie", cookieStr)
	req2.SetHeader("Referer", "https://gemini.google.com/")

	resp, err := req2.Get(EndpointInit + "?hl=en")
	if err != nil {
		return fmt.Errorf("failed to reach gemini app: %w", err)
	}

	bodyBytes := resp.Bytes()
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
			c.log.Info(errMsg)
			return fmt.Errorf("%s", errMsg)
		}
	}

	// Extract build label (try multiple patterns - critical for avoiding bot detection)
	bl := extractBuildLabel(body)

	// Extract session ID (try multiple patterns)
	sid := extractSessionID(body)

	c.log.Debug("Session params extracted",
		zap.String("build_label", bl),
		zap.String("session_id", sid),
		zap.Bool("has_bl", bl != ""),
		zap.Bool("has_sid", sid != ""),
	)

	c.mu.Lock()
	c.at = matches[1]
	c.buildLabel = bl
	c.sessionID = sid
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
	
	// First, add all manually supported models
	for id := range SupportedModels {
		if !uniqueIDs[id] {
			uniqueIDs[id] = true
			newModels = append(newModels, ModelInfo{
				ID:       id,
				Created:  now,
				OwnedBy:  "google",
				Provider: "gemini",
			})
		}
	}

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
				sessionErr := c.RefreshSession()
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

					// When both rotation and session fetch fail, the __Secure-1PSID / 1PSIDTS is likely dead.
					// We pass ErrAccessDenied so that the Client will change its status to Banned and queue it for clear/re-learn
					if c.OnError != nil {
						c.OnError(c.AccountID, fmt.Errorf("background refresh failed, cookie likely dead: %v | %w", sessionErr, ErrAccessDenied))
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
				sessionErr := c.RefreshSession()
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

	c.log.Debug("Sending rotation request", zap.String("url", EndpointRotateCookies))
	
	hClient := reqv3.NewClient().
		ImpersonateChrome().
		SetTimeout(5 * time.Second).
		SetCookieJar(nil)
		
	resp, err := hClient.R().
		SetHeader("Content-Type", "application/json").
		SetHeader("Cookie", cookieStr).
		SetBodyString(strBody).
		Post(EndpointRotateCookies)

	if err != nil {
		c.log.Info("Rotation request failed (network/auth issue)", zap.String("error", err.Error()))
		return fmt.Errorf("failed to call rotation endpoint: %w", err)
	}

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

	targetModel := config.Model
	
	// Strict override for guest-system-backup: Prevent using Pro or custom models that trigger bot blocks
	if c.AccountID == "guest-system-backup" {
		targetModel = "gemini-2.0-flash"
		c.log.Debug("⚠️ Overriding requested model for Guest Mode to avoid anti-bot flags", zap.String("forced_model", targetModel))
	} else if !found && targetModel != "" {
		c.log.Warn("⚠️ Model requested by client is not in confirmed list. Falling back to default gemini-2.0-flash.", zap.String("requested", targetModel))
		targetModel = "gemini-2.0-flash"
	}

	if at == "" && !strings.HasPrefix(c.AccountID, "guest-") {
		return nil, errors.New("client not initialized")
	}
	if at == "" {
		at = "null" // Fallback for guest mode
	}

	mInfo, ok := SupportedModels[targetModel]
	if !ok {
		// If it's a raw hex ID found via regex, we might not have it in SupportedModels mapping.
		// Use it directly as RPCID if it looks like one, otherwise default to Flash.
		if len(targetModel) > 10 && !strings.Contains(targetModel, "-") {
			mInfo = GeminiModelInfo{RPCID: targetModel, CapacityTail: 1}
		} else {
			mInfo = SupportedModels["gemini-2.0-flash"]
		}
	}

	u := uuid.New().String()
	uUpper := strings.ToUpper(u)

	// Build request payload with canonical 69-element structure (as per Python reference)
	inner := make([]interface{}, 69)

	// Index 0: Message content [prompt, role_type, null, file_data, null, null, 0]
	inner[0] = []interface{}{
		prompt,
		0,
		nil,
		nil,
		nil,
		nil,
		0,
	}
	inner[1] = []interface{}{"en"}  // language
	inner[2] = []interface{}{"", "", "", nil, nil, nil, nil, nil, nil, ""} // metadata (DEFAULT_METADATA)
	inner[6] = []interface{}{1}
	inner[7] = 1  // STREAMING_FLAG_INDEX
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

	c.mu.RLock()
	bl := c.buildLabel
	sid := c.sessionID
	c.mu.RUnlock()

	c.muCounter.Lock()
	c.requestCounter += 100000
	reqID := c.requestCounter
	c.muCounter.Unlock()

	// Build headers
	modelHeaderValue := fmt.Sprintf(`[1,null,null,null,"%s",null,null,0,[4],null,null,%d]`, mInfo.RPCID, mInfo.CapacityTail)
	headers := map[string]string{
		"x-goog-ext-525001261-jspb": modelHeaderValue,
		"x-goog-ext-73010989-jspb":  "[0]",
		"x-goog-ext-73010990-jspb":  "[0]",
		"x-goog-ext-525005358-jspb": fmt.Sprintf(`["%s",1]`, uUpper),
	}

	maxAttempts := c.maxRetries
	if maxAttempts <= 0 {
		maxAttempts = 1
	}

	totalStart := time.Now()
	httpStart := time.Now()

	// Note: Busy state is managed by Client.AcquireWorker/release — NOT here.
	// This allows the caller (Client) to hold the worker across multiple operations
	// (e.g., chat session + tool correction retries) without premature release.

	// Send warmup activity to avoid rate limiting (matches Python _send_bard_activity)
	// Send warmup activity to avoid rate limiting (matches Python _send_bard_activity)
	if err := c.sendBardActivity(ctx); err != nil {
		c.log.Warn("Warmup activity failed (silent rejection risk)", zap.Error(err))
	}
	
	c.log.Info("📤 [DIAGNOSTIC] Sending Generation Request", 
		zap.String("account_id", c.AccountID),
		zap.String("bl", bl),
		zap.String("sid", sid),
		zap.Int("req_id", reqID),
	)

	c.ReqMu.RLock()
	request := c.httpClient.R().
		SetContext(ctx).
		SetHeaders(headers).
		SetFormData(formData).
		SetQueryParam("rt", "c").
		SetQueryParam("hl", "en").
		SetQueryParam("_reqid", fmt.Sprintf("%d", reqID))
	
	if bl != "" {
		request.SetQueryParam("bl", bl)
	}
	if sid != "" {
		request.SetQueryParam("f.sid", sid)
	}

	if config.OnProgress != nil {
		request.SetDownloadCallback(func(info reqv3.DownloadInfo) {
			if info.DownloadedSize > 0 {
				config.OnProgress()
			}
		})
	}

	resp, err := request.Post(EndpointGenerate)
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
		body := resp.String()
		body500 := body
		if len(body500) > 500 {
			body500 = body500[:500]
		}
		isHTML := strings.Contains(body, "<!DOCTYPE html") || strings.Contains(body, "<html>")
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			err = ErrAccessDenied
			c.log.Error("🚫 Gemini Access Denied (403 Forbidden). Your cookies are dead or your IP is blocked by Google.",
				zap.Int("status", resp.StatusCode),
				zap.String("account_id", c.AccountID),
			)
		} else if resp.StatusCode == http.StatusTooManyRequests && isHTML {
			err = ErrBotBlocked
			c.log.Error("🛑 Gemini Rate Limited/Bot Blocked (429). Google detected automation.",
				zap.Int("status", resp.StatusCode),
			)
		} else {
			err = fmt.Errorf("generate failed with status: %d", resp.StatusCode)
			c.log.Error("Server returned error status",
				zap.Int("status", resp.StatusCode),
				zap.String("response_body", body500),
			)
		}
		if c.OnError != nil {
			c.OnError(c.AccountID, err)
		}
		return nil, err
	}

	respBody := resp.String()
	if c.log != nil && c.log.Check(zap.DebugLevel, "raw response") != nil {
		c.log.Debug("Raw Response received", zap.Int("bytes", len(respBody)), zap.String("body", respBody))
	}

	parseStart := time.Now()
	result, parseErr := c.parseResponse(respBody)
	parseDuration := time.Since(parseStart)

	// Inject raw payload for debugging and healing even on failure
	if parseErr != nil {
		// If we failed to parse, we might still want to see what we got
		c.log.Warn("Extraction failed, raw response was received", zap.Int("size", len(respBody)))
	}

	if parseErr != nil {
		bodySnippet := respBody
		if len(bodySnippet) > 500 {
			bodySnippet = bodySnippet[:500]
		}
		
		if errors.Is(parseErr, ErrAccessDenied) {
			c.log.Error("🚫 Gemini rejected the request (Access Denied)", 
				zap.Error(parseErr),
				zap.String("raw_response_snippet", bodySnippet),
			)
		} else {
			c.log.Error("Failed to parse response",
				zap.Error(parseErr),
				zap.Int("body_size", len(respBody)),
				zap.String("raw_response_snippet", bodySnippet),
			)
		}
		
		if c.OnError != nil {
			c.OnError(c.AccountID, parseErr)
		}
		
		// Return partial result if we have raw body, to help healing
		if result == nil {
			result = &Response{Metadata: make(map[string]any)}
		}
		result.Metadata["raw_payload"] = respBody
		return result, parseErr
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
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.closed {
		close(c.stopRefresh)
		c.closed = true
	}
	c.healthy = false
	return nil
}

func (c *Worker) Reactivate() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		c.stopRefresh = make(chan struct{})
		c.closed = false
		if c.autoRefresh {
			go c.startAutoRefresh()
		}
	}
	c.healthy = true
}

func (c *Worker) SetBusy(busy bool) {
	c.busyMu.Lock()
	wasBusy := c.isBusy
	c.isBusy = busy
	c.busyMu.Unlock()

	// If transitioning from busy→idle, notify the queue so waiting requests can proceed
	if wasBusy && !busy && c.OnRelease != nil {
		c.OnRelease()
	}
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

// ExtractInnerPayloads parses the raw response stream and returns all inner JSON payload strings
func ExtractInnerPayloads(text string) []string {
	var payloads []string
	lines := strings.Split(text, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if isNumeric(line) {
			continue
		}
		line = strings.TrimPrefix(line, ")]}'")
		if line == "" {
			continue
		}

		var root []interface{}
		if err := json.Unmarshal([]byte(line), &root); err == nil {
			for _, item := range root {
				itemArray, ok := item.([]interface{})
				if !ok || len(itemArray) < 3 {
					// Direct payload fallback (break to avoid duplicate append)
					payloads = append(payloads, line)
					break
				}
				payloadStr, ok := itemArray[2].(string)
				if !ok {
					continue
				}
				// Verify it's a valid JSON array
				var test []interface{}
				if err := json.Unmarshal([]byte(payloadStr), &test); err == nil {
					payloads = append(payloads, payloadStr)
				}
			}
		}
	}
	return payloads
}

func (c *Worker) parseResponse(text string) (*Response, error) {
	var finalResp *Response
	schema := DefaultExtractorSchema()
	if c.SchemaMgr != nil {
		s := c.SchemaMgr.GetSchema()
		schema = &s
	}

	lines := strings.Split(text, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		
		// Skip numeric length prefixes (e.g. "160" or "1549")
		if isNumeric(line) {
			continue
		}

		line = strings.TrimPrefix(line, ")]}'")
		if line == "" {
			continue
		}

		// Detect specific Google internal errors that indicate access denied
		if strings.Contains(line, "wrb.fr") && strings.Contains(line, "[13]") {
			return nil, ErrAccessDenied
		}

		var root []interface{}
		if err := json.Unmarshal([]byte(line), &root); err == nil {
			for _, item := range root {
				itemArray, ok := item.([]interface{})
				if !ok || len(itemArray) < 3 {
					// Fallback: check if this is the payload itself
					resText, cid, rid, rcid, err := ExtractFromPayload(root, *schema)
					if err == nil && resText != "" {
						if finalResp == nil {
							finalResp = &Response{Metadata: make(map[string]any)}
						}
						c.fillResponse(finalResp, resText, cid, rid, rcid, line)
					}
					// Break instead of continue to avoid running this on every non-conforming item
					break
				}

				payloadStr, ok := itemArray[2].(string)
				if !ok {
					continue
				}

				var payload []interface{}
				if err := json.Unmarshal([]byte(payloadStr), &payload); err != nil {
					continue
				}

				resText, cid, rid, rcid, err := ExtractFromPayload(payload, *schema)
				if err == nil && resText != "" {
					if finalResp == nil {
						finalResp = &Response{Metadata: make(map[string]any)}
					}
					c.fillResponse(finalResp, resText, cid, rid, rcid, payloadStr)
				}
			}
		}
	}

	if finalResp != nil {
		if len(finalResp.Images) > 0 {
			c.log.Info("🖼️ Successfully extracted generated images from Gemini", zap.Int("count", len(finalResp.Images)))
		}
		return finalResp, nil
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

// sendBardActivity sends a required warmup request to Google's batchexecute endpoint
// before StreamGenerate calls. Without this, Google rate-limits requests (429) as bot traffic.
// This matches the _send_bard_activity() method in the Python Gemini-API reference client.
func (c *Worker) sendBardActivity(ctx context.Context) error {
	c.mu.RLock()
	at := c.at
	bl := c.buildLabel
	sid := c.sessionID
	lang := "en"
	c.mu.RUnlock()

	if at == "" {
		return nil
	}

	c.muCounter.Lock()
	c.requestCounter += 100000
	reqID := c.requestCounter
	c.muCounter.Unlock()

	// Payload: ESY5D with bard activity settings
	rpcPayload := `[[["bard_activity_enabled"]]]`
	f := [][]interface{}{{"ESY5D", rpcPayload, nil, "generic"}}
	fJSON := marshalNoEscape([]interface{}{f})

	params := map[string]string{
		"rpcids":      "ESY5D",
		"source-path": "/app",
		"hl":          lang,
		"_reqid":      fmt.Sprintf("%d", reqID),
		"rt":          "c",
	}
	if bl != "" {
		params["bl"] = bl
	}
	if sid != "" {
		params["f.sid"] = sid
	}

	c.ReqMu.RLock()
	resp, err := c.httpClient.R().
		SetContext(ctx).
		SetQueryParams(params).
		SetFormData(map[string]string{
			"at":    at,
			"f.req": string(fJSON),
		}).
		SetHeader("x-goog-ext-525001261-jspb", "[1,null,null,null,null,null,null,null,[4]]").
		SetHeader("x-goog-ext-73010989-jspb", "[0]").
		Post(EndpointBatchExec)
	c.ReqMu.RUnlock()

	if err != nil {
		return err
	}
	if resp.StatusCode != 200 {
		return fmt.Errorf("warmup activity failed with status %d", resp.StatusCode)
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

// extractBuildLabel extracts the build label from Gemini's HTML.
// Google stores this as internal key "cfb2h" (not the literal "bl").
// The value looks like "boq_assistant-bard-web-server_20260329.14_p3".
func extractBuildLabel(body string) string {
	// Primary: internal key "cfb2h" used by WIZ_global_data
	if m := regexp.MustCompile(`"cfb2h"\s*:\s*"([^"]+)"`).FindStringSubmatch(body); len(m) > 1 {
		return m[1]
	}
	// Fallback: try the literal "bl" key (some page variants)
	if m := regexp.MustCompile(`"bl"\s*:\s*"(boq[^"]+)"`).FindStringSubmatch(body); len(m) > 1 {
		return m[1]
	}
	// Fallback: any boq_assistant pattern
	if m := regexp.MustCompile(`(boq_assistant-bard-web-server_[0-9a-z._-]+)`).FindStringSubmatch(body); len(m) > 1 {
		return m[1]
	}
	return ""
}

// extractSessionID extracts the session ID from Gemini's HTML.
// Google stores this as internal key "FdrFJe" (not the literal "f.sid").
// The value is a large integer (possibly negative).
func extractSessionID(body string) string {
	// Primary: internal key "FdrFJe" used by WIZ_global_data
	if m := regexp.MustCompile(`"FdrFJe"\s*:\s*"(-?[0-9]+)"`).FindStringSubmatch(body); len(m) > 1 {
		return m[1]
	}
	// Fallback: try literal "f.sid" (some page variants)
	if m := regexp.MustCompile(`"f\.sid"\s*:\s*"(-?[0-9]+)"`).FindStringSubmatch(body); len(m) > 1 {
		return m[1]
	}
	return ""
}

var DefaultHeaders = map[string]string{
"Content-Type":  "application/x-www-form-urlencoded;charset=utf-8",
"Origin":        "https://gemini.google.com",
"Referer":       "https://gemini.google.com/",
"X-Same-Domain": "1",
}

func marshalNoEscape(v interface{}) string {
	var buf strings.Builder
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return "{}"
	}
	return strings.TrimSuffix(buf.String(), "\n")
}
func isNumeric(s string) bool {
	_, err := strconv.Atoi(s)
	return err == nil
}

func (c *Worker) fillResponse(resp *Response, text, cid, rid, rcid, rawPayload string) {
	// Detect new Google safety filters
	if strings.Contains(text, "safety filters are kicking in") ||
		strings.Contains(text, "I can't help with that") ||
		strings.Contains(text, "cannot fulfill this request") {
		// Note: We don't return error here, just set text but callers might check ErrSafetyBlock
		// Actually, let the caller of fillResponse decide, but usually we return ErrSafetyBlock in parseResponse.
	}

	resp.Text = text
	if cid != "" {
		resp.Metadata["cid"] = cid
	}
	if rid != "" {
		resp.Metadata["rid"] = rid
	}
	if rcid != "" {
		resp.Metadata["rcid"] = rcid
	}

	// Always store raw payload for healing purposes
	resp.Metadata["raw_payload"] = rawPayload

	// Extract generated image URLs using regex pattern
	pattern := `https://lh3\.googleusercontent\.com/[a-zA-Z0-9\-_]{50,}(?:=[a-zA-Z0-9\-_]+|/fife/|/image/)[a-zA-Z0-9\-_]*`
	re := regexp.MustCompile(pattern)
	matches := re.FindAllString(rawPayload, -1)

	uniqueMap := make(map[string]bool)
	for _, img := range resp.Images {
		uniqueMap[img.URL] = true
	}

	for _, url := range matches {
		if !uniqueMap[url] {
			uniqueMap[url] = true
			resp.Images = append(resp.Images, Image{
				URL: url,
			})
		}
	}
}
