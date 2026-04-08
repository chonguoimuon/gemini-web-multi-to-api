// cmd/validate_guests/main.go
// Script xác thực các guest platform trong guest_platforms.json bằng cách gọi Gemini API guest mode
// Chạy: go run ./cmd/validate_guests/main.go

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/imroc/req/v3"
	"github.com/tidwall/gjson"
)

// ExtractorSchema mirrors the schema in the main codebase
type ExtractorSchema struct {
	CandidatePath string `json:"candidate_path"`
	TextPath      string `json:"text_path"`
	RCIDPath      string `json:"rcid_path"`
	CIDPath       string `json:"cid_path"`
	RIDPath       string `json:"rid_path"`
}

// PlatformConfig mirrors the main codebase struct
type PlatformConfig struct {
	Name            string          `json:"name"`
	BaseURL         string          `json:"base_url"`
	PayloadTemplate string          `json:"payload_template,omitempty"`
	GJSONPaths      ExtractorSchema `json:"gjson_paths"`
	AtToken         string          `json:"at_token,omitempty"`
	LastLearned     int64           `json:"last_learned"`
	IsValid         bool            `json:"is_valid"`
	FailCount       int             `json:"fail_count"`
	Disabled        bool            `json:"disabled"`
	StrongestModel  string          `json:"strongest_model,omitempty"`
}

// Gemini request constants (mirrors gemini_service.go logic)
const (
	EndpointGenerate = "https://gemini.google.com/_/BardChatUi/data/assistant.lamda.BardFrontendService/StreamGenerate"
)

var DefaultHeaders = map[string]string{
	"Accept":          "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8,application/signed-exchange;v=b3;q=0.7",
	"Accept-Language": "en-US,en;q=0.9",
	"Cache-Control":   "no-cache",
	"Content-Type":    "application/x-www-form-urlencoded;charset=UTF-8",
	"Pragma":          "no-cache",
	"Referer":         "https://gemini.google.com/",
	"Origin":          "https://gemini.google.com",
}

type ValidationResult struct {
	Name    string
	Success bool
	Text    string
	Error   string
}

func main() {
	// 1. Load guest_platforms.json
	dataPath := filepath.Join("data", "guest_platforms.json")
	rawData, err := os.ReadFile(dataPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ Cannot read %s: %v\n", dataPath, err)
		os.Exit(1)
	}

	var platforms map[string]*PlatformConfig
	if err := json.Unmarshal(rawData, &platforms); err != nil {
		fmt.Fprintf(os.Stderr, "❌ Cannot parse JSON: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("📋 Loaded %d platforms from %s\n\n", len(platforms), dataPath)

	// 2. Validate chỉ những platform is_valid=false và không disabled
	var toValidate []*PlatformConfig
	for _, pc := range platforms {
		if !pc.IsValid && !pc.Disabled {
			toValidate = append(toValidate, pc)
		}
	}

	if len(toValidate) == 0 {
		fmt.Println("✅ Tất cả platform đã hợp lệ hoặc bị vô hiệu hóa. Không cần xác thực.")
		return
	}

	fmt.Printf("🔍 Cần xác thực %d platforms: ", len(toValidate))
	for _, pc := range toValidate {
		fmt.Printf("%s ", pc.Name)
	}
	fmt.Println("\n")

	// 3. Thực hiện validation tuần tự (tránh overload bot detection)
	var mu sync.Mutex
	results := make([]ValidationResult, 0)

	for _, pc := range toValidate {
		fmt.Printf("🧪 Đang test platform: %s (%s)\n", pc.Name, pc.BaseURL)

		result := validatePlatform(pc)
		results = append(results, result)

		mu.Lock()
		if result.Success {
			fmt.Printf("   ✅ THÀNH CÔNG - Text: %s\n\n", truncate(result.Text, 100))
			platforms[pc.Name].IsValid = true
			platforms[pc.Name].FailCount = 0
		} else {
			fmt.Printf("   ❌ THẤT BẠI - Error: %s\n\n", result.Error)
		}
		mu.Unlock()

		// Nghỉ giữa các request để tránh rate limit
		time.Sleep(2 * time.Second)
	}

	// 4. Lưu kết quả vào file
	updated, err := json.MarshalIndent(platforms, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ Cannot marshal JSON: %v\n", err)
		os.Exit(1)
	}

	if err := os.WriteFile(dataPath, updated, 0644); err != nil {
		fmt.Fprintf(os.Stderr, "❌ Cannot write file: %v\n", err)
		os.Exit(1)
	}

	// 5. Summary
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("📊 KẾT QUẢ XÁC THỰC:")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	validCount := 0
	for _, r := range results {
		status := "❌ FAIL"
		if r.Success {
			status = "✅ PASS"
			validCount++
		}
		fmt.Printf("   %s  %s\n", status, r.Name)
	}
	fmt.Printf("\n✅ %d/%d platform đã được xác thực thành công\n", validCount, len(results))
	fmt.Printf("💾 Đã cập nhật %s\n", dataPath)
}

// validatePlatform thực hiện HTTP request theo đúng cách Gemini guest mode hoạt động
func validatePlatform(pc *PlatformConfig) ValidationResult {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Tạo HTTP client giả lập Chrome
	client := req.NewClient().
		ImpersonateChrome().
		SetTimeout(60 * time.Second).
		SetCommonHeaders(DefaultHeaders)

	// Bước 1: Lấy SNlM0e token từ Gemini
	atToken, bl, sid, err := getGeminiSessionToken(ctx, client, pc)
	if err != nil {
		return ValidationResult{
			Name:  pc.Name,
			Error: fmt.Sprintf("session token error: %v", err),
		}
	}

	// Bước 2: Gửi request "hello" theo format đúng của Gemini guest
	text, err := sendGeminiGuestRequest(ctx, client, "hello", atToken, bl, sid, pc)
	if err != nil {
		return ValidationResult{
			Name:  pc.Name,
			Error: fmt.Sprintf("request error: %v", err),
		}
	}

	if text == "" {
		return ValidationResult{
			Name:  pc.Name,
			Error: "empty response text",
		}
	}

	return ValidationResult{
		Name:    pc.Name,
		Success: true,
		Text:    text,
	}
}

// getGeminiSessionToken lấy SNlM0e token từ Gemini (guest mode - không cần cookie)
func getGeminiSessionToken(ctx context.Context, client *req.Client, pc *PlatformConfig) (atToken, bl, sid string, err error) {
	// Lấy token từ trang chủ Gemini
	targetURL := pc.BaseURL
	if targetURL == "" {
		targetURL = "https://gemini.google.com/app"
	}

	resp, httpErr := client.R().
		SetContext(ctx).
		SetHeader("Cache-Control", "max-age=0").
		SetHeader("Sec-Fetch-Dest", "document").
		SetHeader("Sec-Fetch-Mode", "navigate").
		SetHeader("Sec-Fetch-Site", "none").
		SetHeader("Sec-Fetch-User", "?1").
		SetHeader("Upgrade-Insecure-Requests", "1").
		Get(targetURL)

	if httpErr != nil {
		return "", "", "", fmt.Errorf("failed to reach %s: %w", targetURL, httpErr)
	}

	body := resp.String()

	// Nếu có at_token được lưu từ lần học trước, dùng nó
	if pc.AtToken != "" && pc.AtToken != "null" {
		atToken = pc.AtToken
	} else {
		// Extract SNlM0e từ body
		atToken = extractSNlM0e(body)
		if atToken == "" {
			// Thử Gemini app nếu base_url không có token
			resp2, err2 := client.R().
				SetContext(ctx).
				Get("https://gemini.google.com/app")
			if err2 == nil {
				atToken = extractSNlM0e(resp2.String())
				bl = extractBuildLabel(resp2.String())
				sid = extractSessionID(resp2.String())
			}
		}
	}

	if atToken == "" {
		// Còn tùy thuộc vào loại platform - nếu không phải Gemini native thì có thể không cần
		atToken = "null" // fallback
	}

	if bl == "" {
		bl = extractBuildLabel(body)
	}
	if sid == "" {
		sid = extractSessionID(body)
	}

	return atToken, bl, sid, nil
}

// sendGeminiGuestRequest gửi request theo format Gemini Web API
func sendGeminiGuestRequest(ctx context.Context, client *req.Client, prompt, atToken, bl, sid string, pc *PlatformConfig) (string, error) {
	schema := pc.GJSONPaths

	// Build Gemini request payload
	inner := make([]interface{}, 69)
	inner[0] = []interface{}{prompt, 0, nil, nil, nil, nil, 0}
	inner[1] = []interface{}{"en"}
	inner[2] = []interface{}{"", "", "", nil, nil, nil, nil, nil, nil, ""}
	inner[6] = []interface{}{1}
	inner[7] = 1
	inner[10] = 1
	inner[11] = 0
	inner[17] = []interface{}{[]interface{}{0}}
	inner[18] = 0
	inner[27] = 1
	inner[30] = []interface{}{4}
	inner[41] = []interface{}{1}
	inner[53] = 0
	inner[59] = "AAAAAAAA-AAAA-AAAA-AAAA-AAAAAAAAAAAA"
	inner[61] = []interface{}{}
	inner[68] = 2

	innerJSON, _ := json.Marshal(inner)
	outer := []interface{}{nil, string(innerJSON)}
	outerJSON, _ := json.Marshal(outer)

	formData := map[string]string{
		"at":    atToken,
		"f.req": string(outerJSON),
	}

	reqBuilder := client.R().
		SetContext(ctx).
		SetHeader("x-goog-ext-525001261-jspb", `[1,null,null,null,"fbb127bbb056c959",null,null,0,[4],null,null,1]`).
		SetHeader("x-goog-ext-73010989-jspb", "[0]").
		SetHeader("x-goog-ext-73010990-jspb", "[0]").
		SetHeader("x-goog-ext-525005358-jspb", `["AAAAAAAA-AAAA-AAAA-AAAA-AAAAAAAAAAAA",1]`).
		SetFormData(formData).
		SetQueryParam("rt", "c").
		SetQueryParam("hl", "en").
		SetQueryParam("_reqid", "100000")

	if bl != "" {
		reqBuilder.SetQueryParam("bl", bl)
	}
	if sid != "" {
		reqBuilder.SetQueryParam("f.sid", sid)
	}

	resp, err := reqBuilder.Post(EndpointGenerate)
	if err != nil {
		return "", fmt.Errorf("POST failed: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(resp.String(), 200))
	}

	body := resp.String()
	if body == "" {
		return "", fmt.Errorf("empty response body")
	}

	// Parse response theo gjson schema
	text := parseGeminiGuestResponse(body, schema)
	return text, nil
}

// parseGeminiGuestResponse parse Gemini streaming response
func parseGeminiGuestResponse(body string, schema ExtractorSchema) string {
	// Gemini trả về multiple JSON lồng nhau, cần split và parse từng phần
	lines := strings.Split(body, "\n")

	var bestText string
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if len(line) < 5 {
			continue
		}

		// Thử parse JSON
		if !gjson.Valid(line) {
			continue
		}

		result := gjson.Parse(line)
		if !result.IsArray() {
			continue
		}

		arr := result.Array()
		if len(arr) == 0 {
			continue
		}

		// Thử extract text theo schema
		candidatesResult := gjson.Get(line, schema.CandidatePath)
		if !candidatesResult.IsArray() || len(candidatesResult.Array()) == 0 {
			continue
		}

		firstCandidate := candidatesResult.Array()[0]
		text := gjson.Get(firstCandidate.Raw, schema.TextPath).String()
		if text != "" && len(text) > len(bestText) {
			bestText = text
		}
	}

	return bestText
}

func extractSNlM0e(body string) string {
	patterns := []string{
		`"SNlM0e":"`,
		`["SNlM0e","`,
	}
	for _, pattern := range patterns {
		idx := strings.Index(body, pattern)
		if idx == -1 {
			continue
		}
		start := idx + len(pattern)
		end := strings.Index(body[start:], "\"")
		if end == -1 {
			continue
		}
		val := body[start : start+end]
		if val != "" {
			return val
		}
	}
	return ""
}

func extractBuildLabel(body string) string {
	patterns := []string{`"FdrFJe":"`, `"bl":"`, `boq_`, `build_label:`}
	for _, pattern := range patterns {
		if idx := strings.Index(body, pattern); idx != -1 {
			start := idx + len(pattern)
			end := strings.Index(body[start:], "\"")
			if end == -1 || end > 100 {
				continue
			}
			val := body[start : start+end]
			if val != "" && len(val) > 3 {
				return val
			}
		}
	}
	return ""
}

func extractSessionID(body string) string {
	patterns := []string{`"sid":"`, `"session_id":"`, `"f.sid":"`, `f.sid=`}
	for _, pattern := range patterns {
		if idx := strings.Index(body, pattern); idx != -1 {
			start := idx + len(pattern)
			end := strings.Index(body[start:], "\"")
			if end == -1 || end > 50 {
				continue
			}
			val := body[start : start+end]
			if val != "" {
				return val
			}
		}
	}
	return ""
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
