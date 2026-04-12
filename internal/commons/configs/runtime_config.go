package configs

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

// RuntimeConfig holds settings that can be changed at runtime via the Admin API
// and are persisted to disk so they survive restarts.
type RuntimeConfig struct {
	AutoDeleteChat      bool `json:"auto_delete_chat"`
	LogRawRequests      bool `json:"log_raw_requests"`
	LogRawSendGemini    bool `json:"log_raw_send_gemini"`
	LogRawGeminiOut     bool `json:"log_raw_gemini_out"`
	LogRawPayloadOut    bool `json:"log_raw_payload_out"`
}

// RuntimeConfigManager manages loading and saving of RuntimeConfig to disk.
// It is safe for concurrent use.
type RuntimeConfigManager struct {
	path string
	mu   sync.RWMutex
	cfg  RuntimeConfig
}

var defaultRuntimeConfig = RuntimeConfig{
	AutoDeleteChat:      false,
	LogRawRequests:      false,
	LogRawSendGemini:    false,
	LogRawGeminiOut:     false,
	LogRawPayloadOut:    false,
}

// NewRuntimeConfigManager creates a manager that loads from (and saves to) the given path.
func NewRuntimeConfigManager(path string) *RuntimeConfigManager {
	m := &RuntimeConfigManager{
		path: path,
		cfg:  defaultRuntimeConfig,
	}
	m.load()
	return m
}

// Get returns a snapshot of the current runtime config.
func (m *RuntimeConfigManager) Get() RuntimeConfig {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.cfg
}

// Update applies a partial update (only non-nil fields) and saves to disk.
func (m *RuntimeConfigManager) Update(autoDelete, logRawReq, logRawSend, logRawGemOut, logRawPayOut *bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if autoDelete != nil {
		m.cfg.AutoDeleteChat = *autoDelete
	}
	if logRawReq != nil {
		m.cfg.LogRawRequests = *logRawReq
	}
	if logRawSend != nil {
		m.cfg.LogRawSendGemini = *logRawSend
	}
	if logRawGemOut != nil {
		m.cfg.LogRawGeminiOut = *logRawGemOut
	}
	if logRawPayOut != nil {
		m.cfg.LogRawPayloadOut = *logRawPayOut
	}
	m.save()
}

// load reads the JSON file from disk. On any error it keeps defaults silently.
func (m *RuntimeConfigManager) load() {
	data, err := os.ReadFile(m.path)
	if err != nil {
		return // file not yet created — keep defaults
	}
	var rc RuntimeConfig
	if err := json.Unmarshal(data, &rc); err == nil {
		m.cfg = rc
	}
}

// save writes the current config to disk. Caller must hold m.mu (write).
func (m *RuntimeConfigManager) save() {
	os.MkdirAll(filepath.Dir(m.path), 0755)
	data, _ := json.MarshalIndent(m.cfg, "", "  ")
	os.WriteFile(m.path, data, 0644)
}
