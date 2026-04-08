package providers

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
	"gemini-web-to-api/internal/commons/configs"
	"github.com/imroc/req/v3"
	"go.uber.org/zap"
)

// GuestWorker handles AI Web requests without authentication (Guest Mode)
type GuestWorker struct {
	httpClient   *req.Client
	log          *zap.Logger
	mu           sync.RWMutex
	client       *Client // Reference back to client for Oracle/Learning
	PlatformName string
}

func NewGuestWorker(cfg *configs.Config, log *zap.Logger, client *Client) *GuestWorker {
	c := req.NewClient().
		ImpersonateChrome().
		SetTimeout(5 * time.Minute).
		SetCommonHeaders(DefaultHeaders)

	return &GuestWorker{
		httpClient: c,
		log:        log,
		client:     client,
	}
}

func (w *GuestWorker) GetConfig() PlatformConfig {
	conf, _ := w.client.multiGuestMgr.GetConfig(w.PlatformName)
	return conf
}

func (w *GuestWorker) Init(ctx context.Context) error {
	config := w.GetConfig()
	w.log.Info("🔧 GuestWorker.Init",
		zap.String("platform", w.PlatformName),
		zap.Bool("is_valid", config.IsValid),
		zap.Bool("disabled", config.Disabled),
		zap.Int("fail_count", config.FailCount),
		zap.String("base_url", config.BaseURL),
		zap.Int64("last_learned", config.LastLearned),
	)
	if !config.IsValid {
		w.log.Warn("⚠️ Guest config is_valid=false. Platform needs re-learning.",
			zap.String("platform", w.PlatformName),
			zap.String("base_url", config.BaseURL),
			zap.Bool("has_payload", config.PayloadTemplate != ""),
		)
		if w.PlatformName == "gemini" || w.PlatformName == "" {
			w.client.TriggerGuestLearning("gemini")
		}
	}
	return nil
}

func (w *GuestWorker) GenerateContent(ctx context.Context, prompt string, options ...GenerateOption) (*Response, error) {
	config := w.GetConfig()

	// 🔍 DIAGNOSTIC: Log full config state before any check
	w.log.Info("📡 GuestWorker.GenerateContent called",
		zap.String("platform", w.PlatformName),
		zap.Bool("is_valid", config.IsValid),
		zap.Bool("disabled", config.Disabled),
		zap.Int("fail_count", config.FailCount),
		zap.String("base_url", config.BaseURL),
		zap.String("at_token", func() string {
			if config.AtToken == "" { return "(empty)" }
			if config.AtToken == "null" { return "null" }
			return config.AtToken[:min(20, len(config.AtToken))] + "..."
		}()),
		zap.String("gjson_candidate", config.GJSONPaths.CandidatePath),
		zap.String("gjson_text", config.GJSONPaths.TextPath),
		zap.String("strongest_model", config.StrongestModel),
		zap.Bool("has_payload_template", config.PayloadTemplate != ""),
	)

	if !config.IsValid {
		w.log.Error("🚫 GuestWorker BLOCKED: is_valid=false. Platform cannot serve requests until re-learned.",
			zap.String("platform", w.PlatformName),
			zap.String("base_url", config.BaseURL),
			zap.String("reason", "Platform config was saved during discovery but validation step was not completed"),
		)
		return nil, fmt.Errorf("guest system [%s] is still learning its structure", w.PlatformName)
	}

	// NOTE: All guest platforms currently route through Gemini's StreamGenerate endpoint.
	// The PayloadTemplate is stored for reference/re-learning only.
	// The platform's BaseURL is the Gemini agent/gem URL used during session learning.
	// The GJSONPaths schema may differ per platform if learned from a non-standard Gemini format.
	w.log.Info("🚀 GuestWorker: Using Gemini StreamGenerate with platform schema",
		zap.String("platform", w.PlatformName),
		zap.String("target", EndpointGenerate),
	)

	tempWorker := &Worker{
		AccountID:  "guest-system-backup", // REQUIRED to bypass "client not initialized" check
		httpClient: w.httpClient,
		log:        w.log,
		healthy:    true,
		at:         config.AtToken, // Use captured 'at' token from learning session
		SchemaMgr:  &SchemaManager{schema: &config.GJSONPaths},
	}

	finalOpts := options
	if config.StrongestModel != "" {
		finalOpts = append(finalOpts, WithModel(config.StrongestModel))
	}

	resp, err := tempWorker.GenerateContent(ctx, prompt, finalOpts...)
	if err != nil {
		isBot := strings.Contains(err.Error(), "status 403") || strings.Contains(err.Error(), "status 429") || strings.Contains(err.Error(), "blocked")
		isParseErr := strings.Contains(err.Error(), "parse") || strings.Contains(err.Error(), "extract")

		w.log.Error("❌ GuestWorker.GenerateContent failed",
			zap.String("platform", w.PlatformName),
			zap.Error(err),
			zap.Bool("is_bot_flag", isBot),
			zap.Bool("is_parse_error", isParseErr),
		)

		if isBot || isParseErr {
			w.log.Warn("🚨 Guest bot flag or extraction failed. Queueing re-learning session.",
				zap.Bool("is_bot", isBot),
				zap.String("platform", w.PlatformName),
			)
			w.client.TriggerGuestLearning(w.PlatformName)
		}
		return nil, err
	}

	w.log.Info("✅ GuestWorker.GenerateContent success",
		zap.String("platform", w.PlatformName),
		zap.String("response_text", func() string {
			if resp != nil { return resp.Text[:min(100, len(resp.Text))] }
			return ""
		}()),
	)
	return resp, nil
}

func min(a, b int) int {
	if a < b { return a }
	return b
}

func (w *GuestWorker) StartChat(options ...ChatOption) ChatSession {
	// Guest mode supports stateless chat by default
	config := w.GetConfig()
	schema := config.GJSONPaths
	model := "gemini-2.0-flash"
	if config.StrongestModel != "" {
		model = config.StrongestModel
	}

	return &GeminiChatSession{
		worker: &Worker{
			AccountID:  "guest-system-backup",
			httpClient: w.httpClient,
			log:        w.log,
			healthy:    true,
			at:         config.AtToken,
			SchemaMgr:  &SchemaManager{schema: &schema},
		},
		model: model,
	}
}

func (w *GuestWorker) Close() error {
	return nil
}

func (w *GuestWorker) GetName() string {
	return "gemini-guest"
}

func (w *GuestWorker) IsHealthy() bool {
	return w.GetConfig().IsValid
}

func (w *GuestWorker) ListModels() []ModelInfo {
	return []ModelInfo{
		{ID: "gemini-2.0-flash", Created: time.Now().Unix(), OwnedBy: w.PlatformName, Provider: "guest-" + w.PlatformName},
		{ID: "gemini-2.0-flash-thinking", Created: time.Now().Unix(), OwnedBy: w.PlatformName, Provider: "guest-" + w.PlatformName},
		{ID: "gemini-2.0-pro-exp", Created: time.Now().Unix(), OwnedBy: w.PlatformName, Provider: "guest-" + w.PlatformName},
		{ID: "gemini-1.5-pro", Created: time.Now().Unix(), OwnedBy: w.PlatformName, Provider: "guest-" + w.PlatformName},
		{ID: "gemini-1.5-flash", Created: time.Now().Unix(), OwnedBy: w.PlatformName, Provider: "guest-" + w.PlatformName},
		{ID: "gemini-3-flash-preview", Created: time.Now().Unix(), OwnedBy: w.PlatformName, Provider: "guest-" + w.PlatformName},
	}
}
