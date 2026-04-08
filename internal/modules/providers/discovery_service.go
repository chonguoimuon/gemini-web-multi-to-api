package providers

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"gemini-web-to-api/internal/commons/configs"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/input"
	"github.com/go-rod/rod/lib/proto"
	"go.uber.org/zap"
)

type DiscoveryService struct {
	cfg       *configs.Config
	log       *zap.Logger
	multiMgr  *MultiGuestManager
	client    *Client // to use Oracle and Launcher
	mu        sync.Mutex
	isRunning bool
}

func NewDiscoveryService(cfg *configs.Config, log *zap.Logger, multiMgr *MultiGuestManager, client *Client) *DiscoveryService {
	return &DiscoveryService{
		cfg:      cfg,
		log:      log,
		multiMgr: multiMgr,
		client:   client,
	}
}

// DiscoverNewPlatforms triggers the learning of the Gemini guest platform exclusively
func (s *DiscoveryService) DiscoverNewPlatforms(ctx context.Context) error {
	s.mu.Lock()
	if s.isRunning {
		s.mu.Unlock()
		s.log.Debug("🔍 Discovery: Already in progress, skipping duplicate session.")
		return nil
	}
	s.isRunning = true
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		s.isRunning = false
		s.mu.Unlock()
	}()

	s.log.Info("📋 Discovery: Initiating targeted learning for Gemini Guest")

	targetURL := "https://gemini.google.com/app"
	name := "gemini"

	// Check if already valid
	if pc, ok := s.multiMgr.GetConfig(name); ok && pc.IsValid && !pc.Disabled {
		return nil
	}

	s.log.Info("🔍 Discovery: Learning/Refreshing Gemini guest", zap.String("url", targetURL))
	err := s.LearnNewPlatform(ctx, targetURL, name)
	if err != nil {
		s.log.Warn("❌ Discovery: Failed to learn Gemini guest", zap.String("url", targetURL), zap.Error(err))
		return err
	}
	
	return nil
}

// LearnNewPlatform launches a browser to analyze and capture the request/response structure of a site
func (s *DiscoveryService) LearnNewPlatform(ctx context.Context, targetURL string, pName string) error {
	s.log.Info("🚀 Discovery: Launching browser for autonomous learning...", zap.String("url", targetURL))

	browser, _, err := s.client.LaunchBrowser(ctx, "discovery-learning", true)
	if err != nil {
		return err
	}
	defer browser.Close()

	var capturedReqBody string
	var capturedRespBody string
	var mu sync.Mutex
	var targetReqID proto.NetworkRequestID
	var reqFinished = make(chan struct{})

	page := browser.Context(ctx).MustPage(targetURL)
	
	// Intercept requests to find AI chat traffic
	go page.EachEvent(func(e *proto.NetworkRequestWillBeSent) {
		// Look for common AI chat endpoints or probe content
		isChat := strings.Contains(e.Request.URL, "conversation") || 
				 strings.Contains(e.Request.URL, "chat") || 
				 strings.Contains(e.Request.URL, "backend-api") ||
				 strings.Contains(e.Request.PostData, "system_probe")

		if isChat && e.Request.Method == "POST" {
			mu.Lock()
			if capturedReqBody == "" {
				s.log.Info("📥 Discovery: Captured POTENTIAL chat request", zap.String("url", e.Request.URL))
				capturedReqBody = e.Request.PostData
				targetReqID = e.RequestID
			}
			mu.Unlock()
		}
	}, func(e *proto.NetworkLoadingFinished) {
		mu.Lock()
		if e.RequestID == targetReqID && targetReqID != "" {
			select {
			case <-reqFinished:
			default:
				close(reqFinished)
			}
		}
		mu.Unlock()
	})()

	// 1. Interaction Sequence
	err = rod.Try(func() {
		s.clearObstacles(page)

		// Wait for landing
		page.MustWaitLoad()
		
		inputSelectors := []string{
			".ProseMirror", "div[contenteditable='true']",
			"textarea[placeholder*='chat'i]", "textarea[placeholder*='message'i]",
			"textarea[placeholder*='prompt'i]", "textarea",
		}

		var inputArea *rod.Element
		for _, sel := range inputSelectors {
			if el, err := page.Timeout(5 * time.Second).Element(sel); err == nil {
				inputArea = el
				break
			}
		}

		if inputArea == nil {
			panic("input field not found")
		}

		inputArea.MustClick().MustInput("Respond ONLY with the exact text 'system_probe_discovery' and nothing else. Output exactly: system_probe_discovery")
		time.Sleep(1 * time.Second)
		page.KeyActions().Press(input.Enter).MustDo()

		// Try clicking send button too
		sendSelectors := []string{
			"button[aria-label*='Send'i]", "button[type='submit']", "svg.lucide-arrow-up",
		}
		for _, sel := range sendSelectors {
			if btn, bErr := page.Timeout(2 * time.Second).Element(sel); bErr == nil {
				_ = btn.Click(proto.InputMouseButtonLeft, 1)
				break
			}
		}

		// Wait for network
		select {
		case <-reqFinished:
		case <-time.After(30 * time.Second):
		}

		if targetReqID != "" {
			res, _ := proto.NetworkGetResponseBody{RequestID: targetReqID}.Call(page)
			capturedRespBody = res.Body
		}
	})

	if err != nil || capturedRespBody == "" || capturedReqBody == "" {
		return fmt.Errorf("autonomous learning failed for %s: bodies empty or interaction error", targetURL)
	}

	// 2. Oracle for paths
	schema, err := s.client.callOracle(ctx, "system_probe_discovery", capturedRespBody)
	if err != nil {
		return err
	}

	// 3. Save Config
	name := pName
	pc := PlatformConfig{
		Name:            name,
		BaseURL:         targetURL,
		PayloadTemplate: capturedReqBody,
		GJSONPaths:      *schema,
		LastLearned:     time.Now().Unix(),
		IsValid:         false,
	}

	s.multiMgr.SaveConfig(pc)

	// 4. Verification
	s.log.Info("🧪 Discovery: Validating new config...", zap.String("platform", name))
	valResult := s.client.ValidateSingleGuestByRawCall(ctx, name)
	if valResult.IsValid {
		s.log.Info("✅ Discovery: Gemini Guest VERIFIED and ENABLED")
	} else {
		s.log.Warn("❌ Discovery: Initial validation failed", zap.String("platform", name), zap.String("error", valResult.Error))
	}

	return nil
}

func (s *DiscoveryService) clearObstacles(page *rod.Page) {
	dismissKeywords := []string{
		"Accept", "Agree", "OK", "Continue", "Dismiss", "Close", "Start Chatting", "Chat Now", "Bắt đầu",
	}

	for _, kw := range dismissKeywords {
		elements, err := page.Elements("button, span, div[role='button'], a")
		if err == nil {
			for _, el := range elements {
				text, _ := el.Text()
				if strings.Contains(strings.ToLower(text), strings.ToLower(kw)) {
					if visible, _ := el.Visible(); visible {
						_ = el.Click(proto.InputMouseButtonLeft, 1)
						time.Sleep(500 * time.Millisecond)
						break
					}
				}
			}
		}
	}

	_, _ = page.Eval(`() => {
		const overlays = document.querySelectorAll('div[class*="overlay"i], div[class*="backdrop"i], div[class*="modal"i]');
		overlays.forEach(el => {
			const style = window.getComputedStyle(el);
			if (style.zIndex && parseInt(style.zIndex) > 50 && style.position === 'fixed') {
				el.style.pointerEvents = 'none';
				el.style.display = 'none';
			}
		});
	}`)
}
