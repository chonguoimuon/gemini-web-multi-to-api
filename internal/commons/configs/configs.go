package configs

import (
	"fmt"
	"os"
	"strconv"

	"github.com/joho/godotenv"
)

type Config struct {
	Gemini GeminiConfig
	Claude ClaudeConfig
	OpenAI OpenAIConfig
	Server    ServerConfig
	RateLimit RateLimitConfig
	MCP       MCPConfig
	LogLevel  string
	// RuntimeCfgMgr manages settings that are changed via Admin API and persisted to disk.
	// Always access AutoDeleteChat / LogRawRequests through this manager or via cfg.Gemini.*
	RuntimeCfgMgr *RuntimeConfigManager
}

type MCPConfig struct {
	Enabled   bool
	Transport string // "stdio" or "sse"
}

type RateLimitConfig struct {
	Enabled     bool
	WindowMs    int
	MaxRequests int
}

type GeminiConfig struct {
	Secure1PSID          string
	Secure1PSIDTS        string
	RefreshInterval      int
	MaxRetries           int
	Cookies              string
	LogRawRequests       bool
	LogRawSendGemini     bool
	LogRawGeminiOut      bool
	LogRawPayloadOut     bool
	AutoDeleteChat       bool
	OracleAPIKeys        string
	GuestRefreshInterval int
	PackContext          int
}

type ClaudeConfig struct {
	APIKey  string
	Model   string
	Cookies string
}

type OpenAIConfig struct {
	APIKey  string
	Model   string
	Cookies string
}

type ServerConfig struct {
	Port        string
	AdminAPIKey string
}

const (
	defaultServerPort            = "4982"
	defaultGeminiRefreshInterval = 5
	defaultGeminiMaxRetries      = 3
	defaultLogLevel              = "info"
)

func New() (*Config, error) {
	// Load .env file if it exists
	_ = godotenv.Load()

	var cfg Config

	// Server
	cfg.Server.Port = getEnv("PORT", defaultServerPort)
	cfg.Server.AdminAPIKey = getEnv("ADMIN_API_KEY", "")
	
	// General
	cfg.LogLevel = getEnv("LOG_LEVEL", defaultLogLevel)

	// Rate Limit
	cfg.RateLimit.Enabled = getEnvBool("RATE_LIMIT_ENABLED", false)
	cfg.RateLimit.WindowMs = getEnvInt("RATE_LIMIT_WINDOW_MS", 60000)
	cfg.RateLimit.MaxRequests = getEnvInt("RATE_LIMIT_MAX_REQUESTS", 10)

	// MCP
	cfg.MCP.Enabled = getEnvBool("MCP_ENABLED", true)
	cfg.MCP.Transport = getEnv("MCP_TRANSPORT", "stdio")

	// Gemini
	cfg.Gemini.Secure1PSID = os.Getenv("GEMINI_1PSID")
	cfg.Gemini.Secure1PSIDTS = os.Getenv("GEMINI_1PSIDTS")
	cfg.Gemini.Cookies = os.Getenv("GEMINI_COOKIES")
	cfg.Gemini.RefreshInterval = getEnvInt("GEMINI_REFRESH_INTERVAL", defaultGeminiRefreshInterval)
	cfg.Gemini.MaxRetries = getEnvInt("GEMINI_MAX_RETRIES", defaultGeminiMaxRetries)
	cfg.Gemini.OracleAPIKeys = os.Getenv("GEMINI_PRO_API_KEYS")
	cfg.Gemini.GuestRefreshInterval = getEnvInt("GEMINI_GUEST_REFRESH_INTERVAL_HOURS", 24)
	cfg.Gemini.PackContext = getEnvInt("GEMINI_PACK_CONTEXT", 32000)

	// Runtime config (persisted to disk — NOT read from .env)
	cfg.RuntimeCfgMgr = NewRuntimeConfigManager("data/runtime_config.json")
	rc := cfg.RuntimeCfgMgr.Get()
	cfg.Gemini.LogRawRequests = rc.LogRawRequests
	cfg.Gemini.LogRawSendGemini = rc.LogRawSendGemini
	cfg.Gemini.LogRawGeminiOut = rc.LogRawGeminiOut
	cfg.Gemini.LogRawPayloadOut = rc.LogRawPayloadOut
	cfg.Gemini.AutoDeleteChat = rc.AutoDeleteChat

	// Validate configuration
	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	return &cfg, nil
}

// Validate checks if the configuration has required values
func (c *Config) Validate() error {
	// Check Server port is valid
	if c.Server.Port == "" {
		c.Server.Port = defaultServerPort
	}

	if _, err := strconv.Atoi(c.Server.Port); err != nil {
		return fmt.Errorf("invalid PORT value: %q (must be a number)", c.Server.Port)
	}

	return nil
}

func getEnv(key, defaultValue string) string {
	if value, exists := os.LookupEnv(key); exists {
		return value
	}
	return defaultValue
}

func getEnvInt(key string, defaultValue int) int {
	valueStr := os.Getenv(key)
	if valueStr == "" {
		return defaultValue
	}
	value, err := strconv.Atoi(valueStr)
	if err != nil {
		return defaultValue
	}
	return value
}

func getEnvBool(key string, defaultValue bool) bool {
	valueStr := os.Getenv(key)
	if valueStr == "" {
		return defaultValue
	}
	value, err := strconv.ParseBool(valueStr)
	if err != nil {
		return defaultValue
	}
	return value
}
