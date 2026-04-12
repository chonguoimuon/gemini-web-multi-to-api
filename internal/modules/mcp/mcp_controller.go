package mcp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/adaptor"
	"go.uber.org/zap"
)

type MCPController struct {
	srv *MCPServer
	log *zap.Logger
}

func NewMCPController(srv *MCPServer, log *zap.Logger) *MCPController {
	return &MCPController{
		srv: srv,
		log: log,
	}
}

// GetMCPTools returns the list of tools available in the MCP server for Swagger testing.
// @Summary List MCP Tools
// @Description Returns a list of tools registered in the MCP server. Use this to verify MCP is active.
// @Tags MCP
// @Accept json
// @Produce json
// @Security BearerAuth
// @Success 200 {array} object
// @Router /mcp/tools [get]
func (h *MCPController) GetMCPTools(c fiber.Ctx) error {
	if h.srv == nil || h.srv.server == nil {
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{
			"error": "MCP server not initialized",
		})
	}

	// Note: We can only list tools if the server is managed by us.
	// Since we are using the official SDK, we can't easily iterate tools from the server struct 
	// without using reflection or keeping a local list. 
	// For Swagger, we will return a static list of what we've implemented.
	
	tools := []fiber.Map{
		{
			"name":        "gemini_chat",
			"description": "General purpose chat with Gemini",
		},
		{
			"name":        "gemini_code_expert",
			"description": "Specialized coding assistant",
		},
		{
			"name":        "gemini_research",
			"description": "Deep research tool",
		},
		{
			"name":        "account_status",
			"description": "Gemini account health tracking",
		},
	}

	return c.JSON(fiber.Map{
		"transport": h.srv.cfg.MCP.Transport,
		"enabled":   h.srv.cfg.MCP.Enabled,
		"tools":     tools,
	})
}

func (h *MCPController) Register(app fiber.Router) {
	// 1. Static tools list for Swagger/Health check
	app.Get("/tools", h.GetMCPTools)

	// 2. Path-Agnostic MCP SSE Routing
	if h.srv.cfg.MCP.Transport == "sse" {
		mcpHandler := h.srv.GetSSEHandler()
		if mcpHandler != nil {
			mcpFinalHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// Anti-Buffering Headers
				w.Header().Set("X-Accel-Buffering", "no")
				w.Header().Set("Cache-Control", "no-cache")
				w.Header().Set("Connection", "keep-alive")

				// --- INTERNAL PATH REWRITE ---
				// Map Fiber path back to what the MCP SDK expects.
				if r.Method == http.MethodGet {
					r.URL.Path = "/"
				} else {
					// Strip /mcp prefix to match SDK's internal mux (usually / or /messages)
					r.URL.Path = strings.TrimPrefix(r.URL.Path, "/mcp")
					if r.URL.Path == "" {
						r.URL.Path = "/"
					}
				}

				// Log request body for POST to see what's happening
				if r.Method == http.MethodPost {
					bodyBytes, _ := io.ReadAll(r.Body)
					r.Body = io.NopCloser(bytes.NewBuffer(bodyBytes))

					// INTERCEPT LOG: Pretty print incoming payload
					var prettyReq bytes.Buffer
					if err := json.Indent(&prettyReq, bodyBytes, "", "  "); err == nil {
						h.log.Info("=========== [MCP 📥 NEW INCOMING JSON-RPC MESSAGE] ===========")
						h.log.Info(fmt.Sprintf("Payload:\n%s", prettyReq.String()))
					}

					// --- STATELESS FALLBACK ---
					// If no sessionId is provided, it's a one-shot request (like initialization)
					if r.URL.Query().Get("sessionId") == "" && r.URL.Query().Get("sessionid") == "" {
						h.log.Info("Handling stateless MCP request", zap.String("path", r.URL.Path))
						resp, err := h.srv.HandleOneShotRequest(r.Context(), bodyBytes)
						if err != nil {
							h.log.Error("Stateless MCP error", zap.Error(err))
							// We can't use http.Error because h.srv.HandleOneShotRequest already 
							// returns a JSON-RPC error response in resp if it handles the error.
							// But if resp is nil, we should return a 500.
							if resp == nil {
								http.Error(w, err.Error(), http.StatusInternalServerError)
								return
							}
						}
						w.Header().Set("Content-Type", "application/json")
						json.NewEncoder(w).Encode(resp)
						return
					}
				}

				// Wrap ResponseWriter to capture status and outgoing body
				iw := &interceptWriter{ResponseWriter: w, status: 200, log: h.log}

				// Execute SDK Handler
				mcpHandler.ServeHTTP(iw, r)

				// Log general bridge result if needed (Optional summary)
				if r.Method != http.MethodPost {
					h.log.Info("MCP SSE Route Established", zap.String("method", r.Method), zap.Int("status", iw.status))
				}
			})

			h.log.Info("MCP SSE Bridge Active at /mcp")

			// Catch all under /mcp
			app.All("/*", adaptor.HTTPHandler(mcpFinalHandler))
			app.All("/", adaptor.HTTPHandler(mcpFinalHandler))
		}
	}
}

type interceptWriter struct {
	http.ResponseWriter
	status int
	log    *zap.Logger
}

func (w *interceptWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *interceptWriter) Write(b []byte) (int, error) {
	// --- SSE ENDPOINT BUGFIX ---
	// If the SDK tells the client to POST to "/" or "/messages", 
	// we must prepend "/mcp" so Fiber can route it back correctly.
	if bytes.Contains(b, []byte("data: /?sessionid=")) {
		b = bytes.ReplaceAll(b, []byte("data: /?sessionid="), []byte("data: /mcp/?sessionid="))
	}
	if bytes.Contains(b, []byte("data: /messages?sessionid=")) {
		b = bytes.ReplaceAll(b, []byte("data: /messages?sessionid="), []byte("data: /mcp/messages?sessionid="))
	}

	// INTERCEPT LOG: Print outgoing SSE payload (Ignore blank PINGs to avoid log spam)
	outStr := string(b)
	if len(outStr) > 10 && outStr != ":ping\n\n" { 
		w.log.Info("=========== [MCP 📤 OUTGOING SSE RESPONSE] ===========")
		w.log.Info(fmt.Sprintf("Payload:\n%s", outStr))
	}
	
	// Pass through to actual writer
	return w.ResponseWriter.Write(b)
}

func (w *interceptWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
