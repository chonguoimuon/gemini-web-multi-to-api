package providers

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// PlatformConfig stores the learned structure for a specific guest platform
type PlatformConfig struct {
	Name            string          `json:"name"`
	BaseURL         string          `json:"base_url"`
	PayloadTemplate string          `json:"payload_template,omitempty"`
	GJSONPaths      ExtractorSchema `json:"gjson_paths"`
	AtToken         string          `json:"at_token,omitempty"`
	Headers         map[string]string `json:"headers,omitempty"`
	LastLearned     int64           `json:"last_learned"`
	IsValid         bool            `json:"is_valid"`
	// Discovery metadata
	InputSelector  string `json:"input_selector,omitempty"`
	SendSelector   string `json:"send_selector,omitempty"`
	NextAttemptAt   int64           `json:"next_attempt_at"`
	WaitMinutes     int             `json:"wait_minutes"`
	FailCount       int             `json:"fail_count"`
	Disabled        bool            `json:"disabled"`
	StrongestModel  string          `json:"strongest_model,omitempty"`
}

type MultiGuestManager struct {
	Configs map[string]*PlatformConfig `json:"configs"`
	path    string
	mu      sync.RWMutex
}

func NewMultiGuestManager() *MultiGuestManager {
	mgm := &MultiGuestManager{
		Configs: make(map[string]*PlatformConfig),
		path:    filepath.Join("data", "guest_platforms.json"),
	}
	mgm.Load()
	return mgm
}

func (m *MultiGuestManager) GetConfigs() []PlatformConfig {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var configs []PlatformConfig
	for _, c := range m.Configs {
		configs = append(configs, *c)
	}
	return configs
}

func (m *MultiGuestManager) GetConfig(name string) (PlatformConfig, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	c, ok := m.Configs[name]
	if !ok {
		return PlatformConfig{}, false
	}
	return *c, true
}

func (m *MultiGuestManager) SaveConfig(nc PlatformConfig) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Configs[nc.Name] = &nc
	m.Save()
}

func (m *MultiGuestManager) RemoveConfig(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.Configs, name)
	m.Save()
}

func (m *MultiGuestManager) Load() {
	m.mu.Lock()
	defer m.mu.Unlock()
	data, err := os.ReadFile(m.path)
	if err != nil {
		return
	}
	var configs map[string]*PlatformConfig
	if err := json.Unmarshal(data, &configs); err == nil {
		m.Configs = configs
	}
}

func (m *MultiGuestManager) Save() {
	os.MkdirAll(filepath.Dir(m.path), 0755)
	data, _ := json.MarshalIndent(m.Configs, "", "  ")
	os.WriteFile(m.path, data, 0644)
}

func (m *MultiGuestManager) GetValidConfigs() []PlatformConfig {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var valid []PlatformConfig
	for _, c := range m.Configs {
		if c.IsValid && !c.Disabled {
			valid = append(valid, *c)
		}
	}
	return valid
}

func (m *MultiGuestManager) Reset(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if c, ok := m.Configs[name]; ok {
		c.IsValid = false
		c.FailCount = 0
		c.Disabled = false
		c.WaitMinutes = 1
		c.NextAttemptAt = 0
		m.Save()
	}
}

func (m *MultiGuestManager) ResetWaitInterval(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if c, ok := m.Configs[name]; ok {
		c.WaitMinutes = 1
		c.FailCount = 0
		c.NextAttemptAt = 0
		m.Save()
	}
}

func (m *MultiGuestManager) ResetFailCount(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if c, ok := m.Configs[name]; ok {
		c.FailCount = 0
		c.Disabled = false
		c.WaitMinutes = 1
		c.NextAttemptAt = 0
		m.Save()
	}
}

func (m *MultiGuestManager) ResetAllFailCounts() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, c := range m.Configs {
		c.FailCount = 0
	}
	m.Save()
}

func (m *MultiGuestManager) Invalidate(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if c, ok := m.Configs[name]; ok {
		c.IsValid = false
		c.FailCount++

		// Tăng thời gian chờ progressive: 1, 2, 3, 4, 5... phút
		c.WaitMinutes = c.FailCount
		if c.WaitMinutes < 1 {
			c.WaitMinutes = 1
		}

		// Giới hạn thời gian chờ tối đa (ví dụ 60 phút) để tránh đợi quá lâu
		if c.WaitMinutes > 60 {
			c.WaitMinutes = 60
		}

		c.NextAttemptAt = time.Now().Unix() + int64(c.WaitMinutes*60)
		c.Disabled = false // Đảm bảo không bao giờ bị disable vĩnh viễn

		m.Save()
	}
}

func (m *MultiGuestManager) CountValidConfigs() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	count := 0
	for _, c := range m.Configs {
		if c.IsValid && !c.Disabled {
			count++
		}
	}
	return count
}
