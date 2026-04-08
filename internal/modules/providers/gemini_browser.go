package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/input"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/proto"
	"go.uber.org/zap"
)

// ClearBot uses a browser to interact with Gemini Web and clear bot flags
func (c *Client) ClearBot(ctx context.Context, accountID string) error {
	c.mu.RLock()
	acc, ok := c.accountsMap[accountID]
	c.mu.RUnlock()

	if !ok {
		return fmt.Errorf("account %s not found", accountID)
	}

	c.log.Info("🤖 ATTEMPTING BOT CLEARANCE", zap.String("id", accountID))

	browser, _, err := c.LaunchBrowser(ctx, accountID, runtime.GOOS != "windows")
	if err != nil {
		return err
	}
	defer browser.Close()

	profileDir := filepath.Join("data", "browser_profiles", accountID)

	c.log.Info("🌐 Opening Gemini App page...", zap.String("id", accountID))
	// 3. Set hard timeout of 180s (3 minutes) for the entire session
	browserCtx, cancel := context.WithTimeout(ctx, 180*time.Second)
	defer cancel()

	page := browser.Context(browserCtx).MustPage("https://gemini.google.com/app")

	// 4. Set Cookies
	cookies := []*proto.NetworkCookieParam{
		{
			Name:     "__Secure-1PSID",
			Value:    acc.Secure1PSID,
			Domain:   ".google.com",
			Path:     "/",
			Secure:   true,
			HTTPOnly: true,
		},
	}
	if acc.Secure1PSIDTS != "" {
		cookies = append(cookies, &proto.NetworkCookieParam{
			Name:     "__Secure-1PSIDTS",
			Value:    acc.Secure1PSIDTS,
			Domain:   ".google.com",
			Path:     "/",
			Secure:   true,
			HTTPOnly: true,
		})
	}

	err = page.SetCookies(cookies)
	if err != nil {
		return fmt.Errorf("failed to set cookies: %w", err)
	}

	// 4. Reload to apply cookies
	err = page.Reload()
	if err != nil {
		return fmt.Errorf("failed to reload page: %w", err)
	}

	// 5. Interaction Loop
	err = rod.Try(func() {
		// Wait for chat box. Selectors for Gemini often change, so we try multiple.
		selectors := []string{
			"div[contenteditable='true']",
			"textarea[aria-label='Chat box']",
			"textarea[placeholder*='Enter a prompt']",
			".ql-editor", // Common for rich editors
		}

		var inputArea *rod.Element
		for _, sel := range selectors {
			el, err := page.Timeout(10 * time.Second).Element(sel)
			if err == nil {
				inputArea = el
				break
			}
		}

		if inputArea == nil {
			panic("chat input not found after 10s")
		}

		inputArea.MustInput("hãy trả lời 'ok'")
		
		// --- HUMAN-LIKE BEHAVIOR: Mouse Movements & Scrolling ---
		c.log.Info("🖱️ Simulating human mouse movements and scrolling...", zap.String("id", accountID))
		for i := 0; i < 3; i++ {
			p := proto.Point{X: float64(100 + i*100), Y: float64(100 + i*100)}
			page.Mouse.MoveTo(p)
			page.Mouse.Scroll(0, 300, 10)
			
			// Click somewhere safe to simulate interaction
			inputArea.MustClick()
			time.Sleep(2 * time.Second)
		}

		// Send button
		sendSelectors := []string{
			"button[aria-label='Send message']",
			"button.send-button",
			"button[type='submit']",
		}

		var sendBtn *rod.Element
		for _, sel := range sendSelectors {
			el, err := page.Timeout(5 * time.Second).Element(sel)
			if err == nil {
				sendBtn = el
				break
			}
		}

		if sendBtn != nil {
			sendBtn.MustClick()
		} else {
			// Try Enter if button not found using input.Enter from the imported package
			inputArea.MustKeyActions().Press(input.Enter).MustDo()
		}

		// --- HUMAN-LIKE BEHAVIOR: Natural Reading Wait ---
		c.log.Info("⏳ Staying on page for 45s to simulate human reading/interaction...", zap.String("id", accountID))
		time.Sleep(45 * time.Second)

		// Capture a final debug screenshot so the user can see Gemini's response
		if runtime.GOOS != "windows" {
			screenshotPath := filepath.Join(profileDir, "debug_clear_bot.png")
			page.MustScreenshot(screenshotPath)
			c.log.Info("📸 Final debug screenshot saved after 60s session. Check this to see Gemini's response!", zap.String("path", screenshotPath))
		}
	})

	if err != nil {
		c.log.Error("❌ BOT CLEARANCE FAILED", zap.String("id", accountID), zap.Error(err))
		return err
	}

	c.log.Info("✅ BOT CLEARANCE SUCCESSFUL for account", zap.String("id", accountID))
	c.ReportSuccess(accountID)
	return nil
}

func (c *Client) LaunchBrowser(ctx context.Context, profileID string, headless bool) (*rod.Browser, string, error) {
	// 1. Setup Isolated Sandbox Directory
	profileDir := filepath.Join("data", "browser_profiles", profileID)
	_ = os.MkdirAll(profileDir, 0755)

	// 2. Determine Browser Path
	bp := getBrowserPath()
	if bp == "" && runtime.GOOS == "linux" {
		bp = "/usr/bin/chromium-browser" // Alpine path
	}

	c.log.Info("🚀 LaunchBrowser: Configuration phase", 
		zap.String("profile_id", profileID),
		zap.String("browser_path", bp),
		zap.String("os", runtime.GOOS),
		zap.Bool("headless", headless))

	l := launcher.New().
		Leakless(runtime.GOOS != "windows").
		UserDataDir(profileDir)

	if bp != "" {
		l.Bin(bp)
	}

	// 3. Set Base Flags
	l.Set("disable-gpu").
		Set("disable-dev-shm-usage").
		Set("disable-notifications").
		Set("disable-infobars").
		Set("disable-popup-blocking").
		Set("mute-audio").
		Set("disable-extensions").
		Set("no-first-run").
		Set("no-default-browser-check").
		Set("password-store", "basic")

	// 4. OS-Specific Flags
	if runtime.GOOS == "windows" {
		// On Windows, some generic sandbox flags can cause issues with specific Chrome versions
		l.Headless(false) // Never headless on Windows
		c.log.Debug("🪟 Launching in WINDOWED mode (OS: Windows)")
	} else {
		// Linux-specific sandboxing and zygote flags
		l.Set("no-sandbox").
			Set("disable-setuid-sandbox").
			Set("no-zygote").
			Headless(headless)
		c.log.Debug("🐧 Launching in LINUX mode (headless=" + fmt.Sprint(headless) + ")")
	}

	// 5. Additional Performance/Stability Flags
	l.Set("window-size", "360,640").
		Set("disable-background-networking").
		Set("disable-sync").
		Set("disable-translate").
		Set("disable-component-update").
		Set("disable-client-side-phishing-detection").
		Set("safebrowsing-disable-download-protection").
		Set("disable-features", "TranslateUI,IsolateOrigins,AutofillServerCommunication,PrivacySandboxSettings4").
		Set("disable-ipc-flooding-protection").
		Set("use-mock-keychain")

	// Manual safety timeout for the entire launch process
	done := make(chan struct{})
	var browserURL string
	var err error
	
	go func() {
		browserURL, err = l.Launch()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(45 * time.Second):
		return nil, "", fmt.Errorf("browser launch TIMED OUT after 45s")
	case <-ctx.Done():
		return nil, "", ctx.Err()
	}

	if err != nil {
		return nil, "", fmt.Errorf("failed to launch chromium: %w", err)
	}

	browser := rod.New().ControlURL(browserURL).MustConnect()
	return browser, browserURL, nil
}

// LearnGuestStructure launches a guest browser session to capture the current Gemini Web request/response format.
// urlList: danh sách các URL để thử, sẽ chọn ngẫu nhiên và thử tuần tự (1 browser tại 1 thời điểm).
// Nếu urlList rỗng, mặc định dùng https://gemini.google.com/app.
func (c *Client) LearnGuestStructure(ctx context.Context, urlList ...string) (*PlatformConfig, error) {
	// Xây dựng danh sách URL để thử: mặc định là Gemini guest URL
	candidates := []string{"https://gemini.google.com/app"}
	if len(urlList) > 0 && len(urlList[0]) > 0 {
		candidates = urlList
	}

	// Xáo trộn ngẫu nhiên để cân bằng tải (load balancing)
	shuffled := make([]string, len(candidates))
	copy(shuffled, candidates)
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	rng.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })

	c.log.Info("🧠 STARTING GUEST STRUCTURE LEARNING...",
		zap.Int("url_count", len(shuffled)),
		zap.Strings("url_list", shuffled))

	// Create a 3-minute hard timeout for the entire learning process
	learningCtx, cancel := context.WithTimeout(ctx, 180*time.Second)
	defer cancel()

	// Thử tuần tự từng URL - KHÔNG mở song song browser
	var lastErr error
	for i, targetURL := range shuffled {
		// Kiểm tra context (có bao gồm timeout 3 phút)
		if learningCtx.Err() != nil {
			return nil, fmt.Errorf("guest structure learning TIMED OUT after 3 minutes: %w", learningCtx.Err())
		}
		
		c.log.Info("🌐 Trying guest URL",
			zap.Int("index", i+1),
			zap.Int("total", len(shuffled)),
			zap.String("url", targetURL))

		result, err := c.learnGuestFromURL(learningCtx, targetURL)
		if err == nil && result != nil {
			c.log.Info("✅ Guest learning SUCCESS", zap.String("url", targetURL))
			return result, nil
		}
		lastErr = err
		c.log.Warn("⚠️ Guest learning FAILED for URL, trying next...",
			zap.String("url", targetURL),
			zap.Error(err))

		// Kiểm tra context trước khi thử URL tiếp theo
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}

	return nil, fmt.Errorf("all %d guest URLs failed, last error: %w", len(shuffled), lastErr)
}

// learnGuestFromURL thực hiện quá trình học cấu trúc guest từ một URL cụ thể.
// Browser được mở và ĐÓNG HOÀN TOÀN trong hàm này trước khi trả về.
func (c *Client) learnGuestFromURL(ctx context.Context, geminiURL string) (*PlatformConfig, error) {
	// Apply 3-minute timeout to the specific URL learn session within the overall sequence
	urlCtx, cancel := context.WithTimeout(ctx, 180*time.Second)
	defer cancel()

	// Mở browser - SẼ ĐÓNG HOÀN TOÀN khi hàm này kết thúc (defer)
	// Trên Linux/VPS, headless luôn là true để tương thích terminal
	isHeadless := runtime.GOOS != "windows"
	
	browser, _, err := c.LaunchBrowser(urlCtx, "guest-learning", isHeadless)
	if err != nil {
		return nil, err
	}
	// Đảm bảo browser đóng HOÀN TOÀN trước khi hàm trả về
	defer func() {
		browser.Close()
		c.log.Info("🔒 Browser closed completely", zap.String("url", geminiURL))
	}()

	// 2. Set up interception — code bên dưới chạy trong learnGuestFromURL
	var capturedReqBody string
	var capturedRespBody string
	var mu sync.Mutex
	var targetReqID proto.NetworkRequestID
	var reqFinished = make(chan struct{})

	page := browser.Context(urlCtx).MustPage(geminiURL)

	// Auto-dismiss common JS dialogs (alert, confirm, prompt)
	go page.EachEvent(func(e *proto.PageJavascriptDialogOpening) {
		c.log.Info("🚫 Auto-dismissing JS dialog", zap.String("message", e.Message))
		_ = proto.PageHandleJavaScriptDialog{Accept: false}.Call(page)
	})()

	// Intercept BardChat requests
	go page.EachEvent(func(e *proto.NetworkRequestWillBeSent) {
		if strings.Contains(e.Request.URL, "BardChat") || strings.Contains(e.Request.URL, "GenerateContent") {
			// Ensure we are capturing the response that actually contains our probe content
			if strings.Contains(e.Request.PostData, "system_probe_") {
				mu.Lock()
				if capturedReqBody == "" {
					c.log.Info("📥 Intercepted PROBE REQUEST payload", zap.Int("size", len(e.Request.PostData)))
					capturedReqBody = e.Request.PostData
					targetReqID = e.RequestID
				}
				mu.Unlock()
			}
		}
	}, func(e *proto.NetworkResponseReceived) {
		// Headers received
	}, func(e *proto.NetworkLoadingFinished) {
		mu.Lock()
		if e.RequestID == targetReqID && targetReqID != "" {
			c.log.Info("🏁 Network Loading FINISHED for target request", zap.String("id", string(targetReqID)))
			// Only close if not already closed
			select {
			case <-reqFinished:
			default:
				close(reqFinished)
			}
		}
		mu.Unlock()
	})()

	// 3. Send Probe Message 1: "SYSTEM_PROBE_OK"
	c.log.Info("📝 Sending FIRST PROBE MESSAGE: SYSTEM_PROBE_OK...")
	err = rod.Try(func() {
		// Wait for input
		inputArea := page.Timeout(20 * time.Second).MustElement("div[contenteditable='true']")
		inputArea.MustClick().MustInput("Please reply with the exact text 'system_probe_ok'. Do not add any other words.")
		
		// Send
		page.KeyActions().Press(input.Enter).MustDo()

		// Wait for LoadingFinished
		select {
		case <-reqFinished:
			c.log.Info("✅ FIRST Probe Response fully loaded")
		case <-time.After(50 * time.Second):
			c.log.Warn("⏳ Timeout waiting for NetworkLoadingFinished (First Probe)")
		}

		// Pull body safely
		mu.Lock()
		currID := targetReqID
		mu.Unlock()
		if currID != "" {
			res, rerr := proto.NetworkGetResponseBody{RequestID: currID}.Call(page)
			if rerr == nil {
				mu.Lock()
				capturedRespBody = res.Body
				mu.Unlock()
				c.log.Debug("📤 Full RAW RESPONSE captured (First Probe)", zap.Int("size", len(res.Body)))
				c.log.Debug("📄 RAW RESPONSE DATA (FULL):", zap.String("content", res.Body))
			} else {
				c.log.Error("❌ Failed to get response body", zap.Error(rerr))
			}
		}
	})

	if err != nil {
		return nil, fmt.Errorf("interaction failed during guest learning: %w", err)
	}

	mu.Lock()
	reqBody := capturedReqBody
	respBody := capturedRespBody
	mu.Unlock()

	if reqBody == "" || respBody == "" {
		return nil, fmt.Errorf("failed to capture guest request/response within timeout")
	}

	c.log.Info("🔍 Captured Guest Probe Data. Sending to Oracle for path analysis...")

	innerPayloads := ExtractInnerPayloads(respBody)
	oracleInput := respBody
	if len(innerPayloads) > 0 {
		// Default to the last payload (usually contains the text candidates) rather than the first (which is metadata)
		oracleInput = innerPayloads[len(innerPayloads)-1]
		for _, p := range innerPayloads {
			if strings.Contains(strings.ToLower(p), "system_probe_ok") {
				oracleInput = p
				break
			}
		}
	}

	// 4. Consult Oracle for Schema
	schema, err := c.callOracle(ctx, oracleInput, "system_probe_ok")
	if err != nil {
		return nil, fmt.Errorf("oracle consultation failed for guest: %w", err)
	}
	c.log.Info("🔮 Oracle suggested GJSON paths", zap.Any("schema", schema))

	// 5. SECOND PROBE: Verification with "system_probe_hello"
	c.log.Info("🧪 VERIFICATION STEP: Sending SECOND PROBE MESSAGE: SYSTEM_PROBE_HELLO...")
	
	// Reset capture state for verification
	mu.Lock()
	capturedReqBody = ""
	capturedRespBody = ""
	targetReqID = ""
	reqFinished = make(chan struct{})
	mu.Unlock()

	err = rod.Try(func() {
		// Wait for input area again to be safe
		inputArea := page.Timeout(10 * time.Second).MustElement("div[contenteditable='true']")
		
		// Clear and input using a more robust method
		inputArea.MustClick()
		// Re-find and input to avoid stale element
		page.KeyActions().Press(input.ControlLeft).Press('a').Release('a').Release(input.ControlLeft).Press(input.Backspace).MustDo()
		inputArea.MustInput("Please reply with the exact text 'system_probe_hello'. Do not add any other words.")
		
		page.KeyActions().Press(input.Enter).MustDo()

		// Wait for LoadingFinished
		select {
		case <-reqFinished:
			c.log.Info("✅ Verification Response fully loaded")
		case <-time.After(50 * time.Second):
			c.log.Warn("⏳ Timeout waiting for Verification NetworkLoadingFinished")
		}

		// Pull verification body
		mu.Lock()
		vID := targetReqID
		mu.Unlock()
		if vID != "" {
			res, rerr := proto.NetworkGetResponseBody{RequestID: vID}.Call(page)
			if rerr == nil {
				mu.Lock()
				capturedRespBody = res.Body
				mu.Unlock()
				c.log.Debug("🧪 Full Verification RAW RESPONSE captured", zap.Int("size", len(res.Body)))
				c.log.Debug("📄 VERIFICATION RAW DATA (FULL):", zap.String("content", res.Body))
			}
		}
	})

	if err != nil {
		return nil, fmt.Errorf("verification interaction failed: %w", err)
	}

	mu.Lock()
	verifyRespBody := capturedRespBody
	mu.Unlock()

	if verifyRespBody == "" {
		return nil, fmt.Errorf("failed to capture verification response")
	}

	// Extract and Verify - Handle Bard's streaming
	verifyPayloads := ExtractInnerPayloads(verifyRespBody)
	
	verified := false
	if len(verifyPayloads) > 0 {
		for _, p := range verifyPayloads {
			var payload []interface{}
			if json.Unmarshal([]byte(p), &payload) == nil {
				text, _, _, _, exErr := ExtractFromPayload(payload, *schema)
				if exErr == nil && strings.Contains(strings.ToLower(text), "system_probe_hello") {
					verified = true
					break
				}
			}
		}
	}

	if verified {
		c.log.Info("✅ VERIFICATION SUCCESS! Extracted 'system_probe_hello' from guest response via JSON paths.")
	} else {
		// Even if greedy search finds it, we don't save the schema because JSON extraction is broken
		if strings.Contains(strings.ToLower(verifyRespBody), "system_probe_hello") {
			c.log.Warn("⚠️ VERIFICATION FAILED: 'system_probe_hello' found in raw response, but NOT via suggested JSON paths. Rejecting schema.")
		} else {
			c.log.Error("❌ VERIFICATION FAILED: 'system_probe_hello' not found in response at all.")
		}
		return nil, fmt.Errorf("guest verification failed: suggested JSON paths do not work")
	}

	// 6. Build Final Result
	atVal := "null"
	cleanTemplate := reqBody
	if strings.Contains(reqBody, "f.req=") {
		params, err := url.ParseQuery(reqBody)
		if err == nil {
			if at := params.Get("at"); at != "" {
				atVal = at
			}
			if fReq := params.Get("f.req"); fReq != "" {
				cleanTemplate = fReq
			}
		}
	}

	config := &PlatformConfig{
		Name:            "gemini",
		BaseURL:         "https://gemini.google.com/app",
		PayloadTemplate: cleanTemplate,
		AtToken:         atVal,
		GJSONPaths:      *schema,
		LastLearned:     time.Now().Unix(),
		IsValid:         true,
	}

	c.log.Info("🎉 GUEST STRUCTURE LEARNED AND VERIFIED SUCCESSFULLY", zap.String("at", atVal))
	return config, nil
}

func getBrowserPath() string {
	paths := []string{
		"C:\\Program Files\\Google\\Chrome\\Application\\chrome.exe",
		"C:\\Program Files (x86)\\Google\\Chrome\\Application\\chrome.exe",
		"/usr/bin/google-chrome",
		"/usr/bin/chromium",
		"/usr/bin/chromium-browser",
		"/usr/bin/chrome",
		"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
	}

	for _, p := range paths {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}
