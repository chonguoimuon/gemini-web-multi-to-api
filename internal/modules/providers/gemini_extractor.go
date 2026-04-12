package providers

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/tidwall/gjson"
)

// DeleteConfig stores the configuration for directly deleting a Gemini chat session.
// This is used to directly hit the Gemini batchexecute endpoint (not MyActivity).
type DeleteConfig struct {
	// RPCID is the RPC method name (e.g. "IoKd5", "LnXdMd", etc.) used in batchexecute
	RPCID string `json:"rpcid"`
	// PayloadTemplate is a Go format string with a single %s for the conversation ID
	PayloadTemplate string `json:"payload_template"`
}

// DefaultDeleteConfig returns the current known working config for direct chat deletion via batchexecute.
func DefaultDeleteConfig() *DeleteConfig {
	return &DeleteConfig{
		RPCID:           "TmdDAd",
		PayloadTemplate: `["%s"]`,
	}
}

// DeleteConfigManager handles loading/saving of the delete configuration.
type DeleteConfigManager struct {
	config *DeleteConfig
	path   string
	mu     sync.RWMutex
}

func NewDeleteConfigManager() *DeleteConfigManager {
	dm := &DeleteConfigManager{
		config: DefaultDeleteConfig(),
		path:   filepath.Join("data", "gemini_delete_config.json"),
	}
	dm.Load()
	return dm
}

func (d *DeleteConfigManager) GetConfig() DeleteConfig {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return *d.config
}

func (d *DeleteConfigManager) UpdateConfig(nc *DeleteConfig) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.config = nc
	d.save()
}

func (d *DeleteConfigManager) Load() {
	data, err := os.ReadFile(d.path)
	if err != nil {
		return
	}
	var cfg DeleteConfig
	if err := json.Unmarshal(data, &cfg); err == nil && cfg.RPCID != "" {
		d.config = &cfg
	}
}

func (d *DeleteConfigManager) save() {
	os.MkdirAll(filepath.Dir(d.path), 0755)
	data, _ := json.MarshalIndent(d.config, "", "  ")
	os.WriteFile(d.path, data, 0644)
}

// ExtractorSchema defines the GJSON paths for Gemini response extraction
type ExtractorSchema struct {
	// CandidatePath is the path to the array of response candidates
	CandidatePath string `json:"candidate_path"`
	// TextPath is the relative path from a candidate to the text content
	TextPath string `json:"text_path"`
	// RCIDPath is the relative path from a candidate to the response candidate ID
	RCIDPath string `json:"rcid_path"`
	// CIDPath is the path to the conversation ID
	CIDPath string `json:"cid_path"`
	// RIDPath is the path to the response ID
	RIDPath string `json:"rid_path"`
}

// DefaultExtractorSchema provides the current known working paths
func DefaultExtractorSchema() *ExtractorSchema {
	return &ExtractorSchema{
		CandidatePath: "4", // payload[4]
		TextPath:      "1.0", // firstCandidate[1][0]
		RCIDPath:      "0", // firstCandidate[0]
		CIDPath:       "1", // payload[1] (can be string or [0]string)
		RIDPath:       "2", // payload[2]
	}
}

type SchemaManager struct {
	schema *ExtractorSchema
	path   string
	mu     sync.RWMutex
}

func NewSchemaManager() *SchemaManager {
	sm := &SchemaManager{
		schema: DefaultExtractorSchema(),
		path:   filepath.Join("data", "gemini_schema.json"),
	}
	sm.Load()
	return sm
}

func (s *SchemaManager) GetSchema() ExtractorSchema {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return *s.schema
}

func (s *SchemaManager) UpdateSchema(ns *ExtractorSchema) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.schema = ns
	s.Save()
}

func (s *SchemaManager) Load() {
	data, err := os.ReadFile(s.path)
	if err != nil {
		return
	}
	var schema ExtractorSchema
	if err := json.Unmarshal(data, &schema); err == nil {
		s.schema = &schema
	}
}

func (s *SchemaManager) Save() {
	os.MkdirAll(filepath.Dir(s.path), 0755)
	data, _ := json.MarshalIndent(s.schema, "", "  ")
	os.WriteFile(s.path, data, 0644)
}

func (s *SchemaManager) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.schema = DefaultExtractorSchema()
	s.Save()
}

// ExtractFromPayload uses GJSON to extract data based on the dynamic schema
func ExtractFromPayload(payload []interface{}, schema ExtractorSchema) (text, cid, rid, rcid string, err error) {
	payloadJSON, _ := json.Marshal(payload)
	payloadStr := string(payloadJSON)

	candidates := gjson.Get(payloadStr, schema.CandidatePath)
	if !candidates.IsArray() || len(candidates.Array()) == 0 {
		return "", "", "", "", fmt.Errorf("no candidates found at path %s", schema.CandidatePath)
	}

	firstCandidate := candidates.Array()[0]
	firstCandidateStr := firstCandidate.Raw

	text = gjson.Get(firstCandidateStr, schema.TextPath).String()
	rcid = gjson.Get(firstCandidateStr, schema.RCIDPath).String()

	cidRes := gjson.Get(payloadStr, schema.CIDPath)
	if cidRes.IsArray() && len(cidRes.Array()) > 0 {
		cid = cidRes.Array()[0].String()
	} else {
		cid = cidRes.String()
	}

	rid = gjson.Get(payloadStr, schema.RIDPath).String()

	return text, cid, rid, rcid, nil
}
