package mcp

import (
	"gemini-web-to-api/internal/commons/configs"
	"github.com/gofiber/fiber/v3"
	"go.uber.org/fx"
	"go.uber.org/zap"
)

var Module = fx.Module("mcp",
	fx.Provide(
		NewMCPServer,
		NewMCPController,
	),
	fx.Invoke(
		RegisterMCPLifecycle,
		RegisterRoutes,
	),
)

func RegisterRoutes(app *fiber.App, c *MCPController, cfg *configs.Config, log *zap.Logger) {
	if !cfg.MCP.Enabled {
		return
	}

	mcpGroup := app.Group("/mcp", func(ctx fiber.Ctx) error {
		method := ctx.Method()
		path := ctx.Path()
		
		if cfg.Server.AdminAPIKey != "" {
			auth := ctx.Get("Authorization")
			if auth != "Bearer "+cfg.Server.AdminAPIKey {
				log.Warn("MCP Unauthorized access", zap.String("method", method), zap.String("path", path))
				return ctx.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "Unauthorized"})
			}
		}
		
		headers := make(map[string]string)
		ctx.Request().Header.VisitAll(func(key, value []byte) {
			headers[string(key)] = string(value)
		})

		log.Info("MCP Incoming Request Details", 
			zap.String("method", method), 
			zap.String("path", path),
			zap.String("query", string(ctx.Request().URI().QueryString())),
			zap.Any("headers", headers))
		return ctx.Next()
	})

	c.Register(mcpGroup)
}
