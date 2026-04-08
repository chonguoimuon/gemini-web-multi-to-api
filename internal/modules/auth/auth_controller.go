package auth

import (
	"fmt"
	"gemini-web-to-api/internal/commons/configs"
	"strings"

	"github.com/gofiber/fiber/v3"
	"go.uber.org/zap"
)

type AuthController struct {
	log *zap.Logger
	cfg *configs.Config
}

func NewAuthController(log *zap.Logger, cfg *configs.Config) *AuthController {
	return &AuthController{
		log: log,
		cfg: cfg,
	}
}

// HandleAuthorize handles the /authorize endpoint (GET) for OAuth2 flow
func (c *AuthController) HandleAuthorize(ctx fiber.Ctx) error {
	clientID := ctx.Query("client_id")
	redirectURI := ctx.Query("redirect_uri")
	state := ctx.Query("state")
	responseType := ctx.Query("response_type")

	c.log.Info("OAuth2 Authorize request received", 
		zap.String("client_id", clientID), 
		zap.String("redirect_uri", redirectURI),
		zap.String("state", state),
		zap.String("response_type", responseType))

	if redirectURI == "" {
		return ctx.Status(fiber.StatusBadRequest).SendString("Missing redirect_uri")
	}

	// For simple mock/proxy server, we just redirect back with a dummy code
	// Most tools expect "code" response type
	authCode := "gemini_mock_auth_code"
	
	// If clientID looks like "Bearer <key>", we can extract the key, 
	// but we'll deal with it in the /token endpoint since the code is just a lookup key.
	// Actually, we'll just pass a generic code.

	delimiter := "?"
	if strings.Contains(redirectURI, "?") {
		delimiter = "&"
	}

	targetURL := fmt.Sprintf("%s%scode=%s&state=%s", redirectURI, delimiter, authCode, state)
	
	c.log.Info("Redirecting back to client", zap.String("url", targetURL))
	return ctx.Redirect().To(targetURL)
}

// HandleToken handles the /token endpoint (POST) for OAuth2 flow
func (c *AuthController) HandleToken(ctx fiber.Ctx) error {
	grantType := ctx.FormValue("grant_type")
	code := ctx.FormValue("code")
	clientID := ctx.FormValue("client_id")
	// clientSecret := ctx.FormValue("client_secret")

	c.log.Info("OAuth2 Token request received", 
		zap.String("grant_type", grantType),
		zap.String("code", code),
		zap.String("client_id", clientID))

	// Determine what to use as the access_token. 
	// If the user's tool sent a specific key in client_id, we'll return it as the access_token.
	// clientID in the User's request was something like 'Bearer 2010201119092012'
	// We'll strip 'Bearer ' if present, or just use the whole thing.
	accessToken := clientID
	if strings.HasPrefix(accessToken, "Bearer ") {
		accessToken = strings.TrimPrefix(accessToken, "Bearer ")
	} else if strings.HasPrefix(accessToken, "Bearer+") {
		accessToken = strings.TrimPrefix(accessToken, "Bearer+")
	}

	// FALLBACK: If clientID is empty or generic, use the server's ADMIN_API_KEY
	if accessToken == "" || accessToken == "undefined" || accessToken == "null" {
		accessToken = c.cfg.Server.AdminAPIKey
	}

	// Standard OAuth2 response
	return ctx.JSON(fiber.Map{
		"access_token": accessToken,
		"token_type":   "Bearer",
		"expires_in":   3600,
	})
}

func (c *AuthController) Register(app *fiber.App) {
	// Root level auth endpoints
	app.Get("/authorize", c.HandleAuthorize)
	app.Post("/token", c.HandleToken)
}
