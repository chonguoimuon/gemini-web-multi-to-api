package main

import (
	"gemini-web-to-api/internal/commons/configs"
	"gemini-web-to-api/internal/modules"
	"gemini-web-to-api/internal/server"
	"gemini-web-to-api/pkg/logger"

	_ "gemini-web-to-api/cmd/swag/docs"

	"go.uber.org/fx"
	"go.uber.org/fx/fxevent"
	"go.uber.org/zap"
)

// @title Gemini Web To API
// @version 1.0
// @description ✨Reverse-engineered API for Gemini web app. It can be used as a genuine API key from OpenAI, Gemini, and Claude.
// @BasePath /
// @securityDefinitions.apikey BearerAuth
// @in header
// @name Authorization
// @description Type "Bearer <your_admin_api_key>"
func main() {
	fx.New(
		fx.Provide(
			configs.New,
			func(cfg *configs.Config) (*zap.Logger, error) {
				return logger.New(cfg.LogLevel, cfg.MCP.Enabled)
			},
		),
		fx.WithLogger(func(log *zap.Logger) fxevent.Logger {
			return &fxevent.ZapLogger{Logger: log.Named("fx")}
		}),
		server.Module,
		modules.Module,
	).Run()
}
