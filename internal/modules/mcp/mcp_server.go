package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"gemini-web-to-api/internal/commons/configs"
	"gemini-web-to-api/internal/modules/gemini"
	"gemini-web-to-api/internal/modules/gemini/dto"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.uber.org/fx"
	"go.uber.org/zap"
)

type MCPServer struct {
	server     *mcp.Server
	sseHandler *mcp.SSEHandler
	gemini     *gemini.GeminiService
	log        *zap.Logger
	cfg        *configs.Config
}

type ChatInput struct {
	Message string `json:"message" jsonschema:"The message to send to Gemini"`
	Model   string `json:"model,omitempty" jsonschema:"Optional model ID (e.g., gemini-2.0-flash)"`
}

type CodeExpertInput struct {
	Message  string `json:"message" jsonschema:"The coding task description"`
	Code     string `json:"code" jsonschema:"The source code context"`
	Language string `json:"language" jsonschema:"Programming language"`
}

type ResearchInput struct {
	Query      string `json:"query" jsonschema:"The search query or research topic"`
	Model      string `json:"model,omitempty" jsonschema:"Optional model ID"`
	Language   string `json:"language,omitempty" jsonschema:"Language for research (e.g., 'en', 'vi')"`
	MaxSources int    `json:"max_sources,omitempty" jsonschema:"Maximum sources to crawl"`
}

func NewMCPServer(gemini *gemini.GeminiService, log *zap.Logger, cfg *configs.Config) *MCPServer {
	if !cfg.MCP.Enabled {
		return &MCPServer{cfg: cfg}
	}

	info := &mcp.Implementation{
		Name:    "gemini-web-multi-to-api",
		Version: "1.0.0",
	}

	s := mcp.NewServer(info, nil)
	
	// Create SSE Handler
	sseHandler := mcp.NewSSEHandler(func(request *http.Request) *mcp.Server {
		return s
	}, nil)

	mcpSrv := &MCPServer{
		server:     s,
		sseHandler: sseHandler,
		gemini:     gemini,
		log:        log,
		cfg:        cfg,
	}

	mcpSrv.registerTools()
	return mcpSrv
}

func (s *MCPServer) registerTools() {
	// 1. Gemini Chat Tool
	mcp.AddTool(s.server, &mcp.Tool{
		Name:        "ask_gemini",
		Description: "General purpose chat with Gemini accounts. Supports long context, reasoning, and multi-turn chat.",
	}, s.handleChat)

	// 2. Gemini Code Expert Tool
	mcp.AddTool(s.server, &mcp.Tool{
		Name:        "coding_expert",
		Description: "Specialized tool for writing, refactoring, and debugging code. Uses optimized prompts for high-quality software engineering output. Provides full code context support.",
	}, s.handleCodeExpert)

	// 3. Gemini Deep Research Tool (Exposed as google_search for better agent synergy)
	mcp.AddTool(s.server, &mcp.Tool{
		Name:        "google_search",
		Description: "Performs deep web research using Google. Returns a comprehensive summary and cited sources. Use this for complex queries requiring recent or broad information.",
	}, s.handleResearch)

	// 4. Account Info Tool
	mcp.AddTool(s.server, &mcp.Tool{
		Name:        "account_status",
		Description: "Returns the status and health of the configured Gemini account pool.",
	}, s.handleAccountStatus)

	// 5. Discover New Guests Tool
	mcp.AddTool(s.server, &mcp.Tool{
		Name:        "discover_guests",
		Description: "Triggers a search for new AI chat platforms that allow guest access for load balancing.",
	}, s.handleDiscoverGuests)
}

func (s *MCPServer) GetSSEHandler() *mcp.SSEHandler {
	return s.sseHandler
}

func (s *MCPServer) handleChat(ctx context.Context, req *mcp.CallToolRequest, input ChatInput) (*mcp.CallToolResult, any, error) {
	s.log.Info("==================================================")
	s.log.Info("📥 NEW REQUEST: MCP ask_gemini")
	if s.cfg.Gemini.LogRawRequests {
		reqBytes, _ := json.MarshalIndent(input, "", "  ")
		s.log.Info(fmt.Sprintf("Request Payload:\n%s", string(reqBytes)))
	}

	if input.Message == "" {
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: "prompt is required"}},
			IsError: true,
		}, nil, nil
	}

	res, err := s.gemini.GenerateContent(ctx, input.Model, dto.GeminiGenerateRequest{
		Contents: []dto.Content{
			{
				Role:  "user",
				Parts: []dto.Part{{Text: input.Message}},
			},
		},
	})

	if err != nil {
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("Gemini error: %v", err)}},
			IsError: true,
		}, nil, nil
	}

	s.log.Info("📤 RETURN TO CLIENT: MCP ask_gemini Result Sent")
	s.log.Info("==================================================")
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: res.Candidates[0].Content.Parts[0].Text}},
	}, nil, nil
}

func (s *MCPServer) handleCodeExpert(ctx context.Context, req *mcp.CallToolRequest, input CodeExpertInput) (*mcp.CallToolResult, any, error) {
	s.log.Info("==================================================")
	s.log.Info("📥 NEW REQUEST: MCP coding_expert")
	if s.cfg.Gemini.LogRawRequests {
		reqBytes, _ := json.MarshalIndent(input, "", "  ")
		s.log.Info(fmt.Sprintf("Request Payload:\n%s", string(reqBytes)))
	}

	if input.Message == "" {
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: "task is required"}},
			IsError: true,
		}, nil, nil
	}

	fullPrompt := fmt.Sprintf("You are an expert software engineer specializing in %s. \nTASK: %s\n\nCODE CONTEXT:\n```%s\n%s\n```\n\nPlease provide the complete updated code or detailed instructions.", input.Language, input.Message, input.Language, input.Code)

	res, err := s.gemini.GenerateContent(ctx, "", dto.GeminiGenerateRequest{
		Contents: []dto.Content{
			{
				Role:  "user",
				Parts: []dto.Part{{Text: fullPrompt}},
			},
		},
	})

	if err != nil {
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("Gemini error: %v", err)}},
			IsError: true,
		}, nil, nil
	}

	s.log.Info("📤 RETURN TO CLIENT: MCP coding_expert Result Sent")
	s.log.Info("==================================================")
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: res.Candidates[0].Content.Parts[0].Text}},
	}, nil, nil
}

func (s *MCPServer) handleResearch(ctx context.Context, req *mcp.CallToolRequest, input ResearchInput) (*mcp.CallToolResult, any, error) {
	s.log.Info("==================================================")
	s.log.Info("📥 NEW REQUEST: MCP google_search")
	if s.cfg.Gemini.LogRawRequests {
		reqBytes, _ := json.MarshalIndent(input, "", "  ")
		s.log.Info(fmt.Sprintf("Request Payload:\n%s", string(reqBytes)))
	}

	if input.Query == "" {
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: "query is required"}},
			IsError: true,
		}, nil, nil
	}

	res, err := s.gemini.DeepResearch(ctx, dto.DeepResearchRequest{
		Query:      input.Query,
		Model:      input.Model,
		Language:   input.Language,
		MaxSources: input.MaxSources,
	})

	if err != nil {
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("Research error: %v", err)}},
			IsError: true,
		}, nil, nil
	}

	summary := fmt.Sprintf("# Research Result: %s\n\n%s\n\n## Sources\n", res.Query, res.Summary)
	for _, src := range res.Sources {
		summary += fmt.Sprintf("- [%s](%s)\n", src.Title, src.URL)
	}

	s.log.Info("📤 RETURN TO CLIENT: MCP google_search Result Sent")
	s.log.Info("==================================================")
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: summary}},
	}, nil, nil
}

func (s *MCPServer) handleAccountStatus(ctx context.Context, req *mcp.CallToolRequest, input struct{}) (*mcp.CallToolResult, any, error) {
	s.log.Info("==================================================")
	s.log.Info("📥 NEW REQUEST: MCP account_status")
	accounts := s.gemini.Client().GetAccounts()
	data, _ := json.MarshalIndent(accounts, "", "  ")
	s.log.Info("📤 RETURN TO CLIENT: MCP account_status Result Sent")
	s.log.Info("==================================================")
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: string(data)}},
	}, nil, nil
}

func (s *MCPServer) handleDiscoverGuests(ctx context.Context, req *mcp.CallToolRequest, input struct{}) (*mcp.CallToolResult, any, error) {
	s.log.Info("==================================================")
	s.log.Info("📥 NEW REQUEST: MCP discover_guests")
	
	// Use background context as discovery might take time
	go s.gemini.Client().GetDiscoverySvc().DiscoverNewPlatforms(context.Background())

	s.log.Info("📤 RETURN TO CLIENT: MCP discover_guests Initiated")
	s.log.Info("==================================================")
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: "Discovery process initiated in the background. Check admin panel for results."}},
	}, nil, nil
}

func RegisterMCPLifecycle(lc fx.Lifecycle, s *MCPServer, log *zap.Logger) {
	if s.server == nil {
		return
	}

	lc.Append(fx.Hook{
		OnStart: func(ctx context.Context) error {
			if s.cfg.MCP.Transport == "stdio" {
				log.Info("Starting MCP server over Stdio")
				go func() {
					if err := s.server.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
						log.Error("MCP server error", zap.Error(err))
					}
				}()
			}
			return nil
		},
		OnStop: func(ctx context.Context) error {
			log.Info("Stopping MCP server")
			return nil
		},
	})
}

// HandleOneShotRequest handles stateless HTTP JSON-RPC requests (no SSE session).
// This is used for clients like Antigravity that may try to initialization via POST directly.
func (s *MCPServer) HandleOneShotRequest(ctx context.Context, body []byte) (interface{}, error) {
	var req struct {
		JSONRPC string `json:"jsonrpc"`
		ID      interface{} `json:"id"`
		Method  string `json:"method"`
		Params  json.RawMessage `json:"params"`
	}

	if err := json.Unmarshal(body, &req); err != nil {
		return nil, fmt.Errorf("invalid JSON-RPC: %w", err)
	}

	// Basic Handlers for one-shot requests
	switch req.Method {
	case "initialize":
		return map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      req.ID,
			"result": map[string]interface{}{
				"protocolVersion": "2024-11-05", // Standard MCP version
				"capabilities": map[string]interface{}{
					"tools": map[string]interface{}{
						"listChanged": true,
					},
					"logging": map[string]interface{}{},
				},
				"serverInfo": map[string]interface{}{
					"name":    "gemini-web-multi",
					"version": "1.1.0",
				},
			},
		}, nil

	case "notifications/initialized":
		// Standard protocol notification, no response required but 200 OK is expected
		return nil, nil

	case "tools/list":
		// Return our tools in MCP format
		return map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      req.ID,
			"result": map[string]interface{}{
				"tools": s.getStaticTools(),
			},
		}, nil

	case "tools/call":
		var params struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &params); err != nil {
			return nil, fmt.Errorf("invalid tool call params: %w", err)
		}

		// Call the appropriate tool handler manually
		var res *mcp.CallToolResult
		var err error

		switch params.Name {
		case "ask_gemini":
			var input ChatInput
			json.Unmarshal(params.Arguments, &input)
			res, _, err = s.handleChat(ctx, nil, input)
		case "coding_expert":
			var input CodeExpertInput
			json.Unmarshal(params.Arguments, &input)
			res, _, err = s.handleCodeExpert(ctx, nil, input)
		case "google_search":
			var input ResearchInput
			json.Unmarshal(params.Arguments, &input)
			res, _, err = s.handleResearch(ctx, nil, input)
		case "account_status":
			res, _, err = s.handleAccountStatus(ctx, nil, struct{}{})
		case "discover_guests":
			res, _, err = s.handleDiscoverGuests(ctx, nil, struct{}{})
		default:
			err = fmt.Errorf("tool not found: %s", params.Name)
		}

		if err != nil {
			return map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      req.ID,
				"error": map[string]interface{}{
					"code":    -32000,
					"message": err.Error(),
				},
			}, nil
		}

		return map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      req.ID,
			"result":  res,
		}, nil

	default:
		// Try to handle other methods vs returning error
		if req.ID == nil {
			// Notification, just ignore
			return nil, nil
		}
		return map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      req.ID,
			"error": map[string]interface{}{
				"code":    -32601,
				"message": fmt.Sprintf("Method not found for stateless request: %s", req.Method),
			},
		}, nil
	}
}

func (s *MCPServer) getStaticTools() []map[string]interface{} {
	return []map[string]interface{}{
		{
			"name":        "ask_gemini",
			"description": "General purpose chat with Gemini accounts. Supports long context and reasoning.",
			"inputSchema": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"message": map[string]interface{}{"type": "string", "description": "The message to send to Gemini"},
					"model":   map[string]interface{}{"type": "string", "description": "Optional model ID (e.g., gemini-2.0-flash)"},
				},
				"required": []string{"message"},
			},
		},
		{
			"name":        "coding_expert",
			"description": "Specialized coding assistant with full context support.",
			"inputSchema": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"message":  map[string]interface{}{"type": "string", "description": "The coding task description"},
					"code":     map[string]interface{}{"type": "string", "description": "The source code context"},
					"language": map[string]interface{}{"type": "string", "description": "Programming language"},
				},
				"required": []string{"message"},
			},
		},
		{
			"name":        "google_search",
			"description": "Performs deep web research using Google. Returns cited sources.",
			"inputSchema": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"query":       map[string]interface{}{"type": "string", "description": "The search query or topic"},
					"model":       map[string]interface{}{"type": "string", "description": "Optional model to use"},
					"language":    map[string]interface{}{"type": "string", "description": "Search language"},
					"max_sources": map[string]interface{}{"type": "integer", "description": "Max sources to crawl"},
				},
				"required": []string{"query"},
			},
		},
		{
			"name":        "account_status",
			"description": "Gemini account health tracking",
			"inputSchema": map[string]interface{}{
				"type": "object",
			},
		},
		{
			"name":        "discover_guests",
			"description": "Trigger discovery of new guest chat platforms",
			"inputSchema": map[string]interface{}{
				"type": "object",
			},
		},
	}
}
