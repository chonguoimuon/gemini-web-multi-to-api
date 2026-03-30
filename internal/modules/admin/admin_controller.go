package admin

import (
	"fmt"
	"gemini-web-to-api/internal/commons/configs"
	"gemini-web-to-api/internal/modules/providers"
	"strings"

	"github.com/gofiber/fiber/v3"
	"go.uber.org/zap"
)

type AdminController struct {
	client *providers.Client // The LoadBalancer AccountManager
	log    *zap.Logger
	cfg    *configs.Config
}

func NewAdminController(client *providers.Client, log *zap.Logger, cfg *configs.Config) *AdminController {
	return &AdminController{
		client: client,
		log:    log,
		cfg:    cfg,
	}
}

// @Summary List All Cookie Accounts
// @Description Returns the status and metrics of all configured Gemini cookie accounts
// @Tags Admin
// @Accept json
// @Produce json
// @Security BearerAuth
// @Success 200 {array} providers.AccountConfig
// @Failure 401 {object} map[string]interface{}
// @Router /admin/v1/accounts [get]
func (c *AdminController) HandleListAccounts(ctx fiber.Ctx) error {
	accounts := c.client.GetAccounts()
	return ctx.JSON(fiber.Map{"data": accounts})
}

type AddAccountRequest struct {
	ID            string `json:"id"`
	Secure1PSID   string `json:"__Secure-1PSID"`
	Secure1PSIDTS string `json:"__Secure-1PSIDTS"`
}

// @Summary Add or Update a Cookie Account
// @Description Injects a new __Secure-1PSID and __Secure-1PSIDTS for load balancing and rotation
// @Tags Admin
// @Accept json
// @Produce json
// @Param request body AddAccountRequest true "Account Details"
// @Security BearerAuth
// @Success 200 {object} map[string]interface{}
// @Failure 401 {object} map[string]interface{}
// @Router /admin/v1/accounts [post]
func (c *AdminController) HandleAddAccount(ctx fiber.Ctx) error {
	var req AddAccountRequest
	if err := ctx.Bind().Body(&req); err != nil {
		return ctx.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "Invalid request body"})
	}

	if req.ID == "" || req.Secure1PSID == "" {
		return ctx.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "ID and __Secure-1PSID are required"})
	}

	c.client.AddAccount(req.ID, req.Secure1PSID, req.Secure1PSIDTS)
	return ctx.JSON(fiber.Map{"status": "success", "message": "Account added via async initialization"})
}

// @Summary Delete a Cookie Account
// @Description Removes an account from the Rotation pool
// @Tags Admin
// @Accept json
// @Produce json
// @Param id path string true "Account ID"
// @Security BearerAuth
// @Success 200 {object} map[string]interface{}
// @Failure 401 {object} map[string]interface{}
// @Router /admin/v1/accounts/{id} [delete]
func (c *AdminController) HandleDeleteAccount(ctx fiber.Ctx) error {
	id := ctx.Params("id")
	if id == "" {
		return ctx.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "Missing account ID"})
	}

	c.client.RemoveAccount(id)
	return ctx.JSON(fiber.Map{"status": "success", "message": fmt.Sprintf("Account %s removed", id)})
}

// @Summary Test Account Rotation
// @Description Forces an immediate check or chat generation on the designated account
// @Tags Admin
// @Accept json
// @Produce json
// @Param id path string true "Account ID"
// @Security BearerAuth
// @Success 200 {object} map[string]interface{}
// @Failure 401 {object} map[string]interface{}
// @Router /admin/v1/accounts/{id}/test [post]
func (c *AdminController) HandleTestAccount(ctx fiber.Ctx) error {
	id := ctx.Params("id")
	
	err := c.client.TestAccount(id)
	if err != nil {
		return ctx.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"status": "error",
			"error":  err.Error(),
		})
	}

	accounts := c.client.GetAccounts()
	var target *providers.AccountConfig
	for i := range accounts {
		if accounts[i].ID == id {
			target = &accounts[i]
			break
		}
	}

	if target == nil {
		return ctx.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "Account not found"})
	}

	return ctx.JSON(fiber.Map{
		"status":  "success", 
		"message": "Account tested successfully and is healthy",
		"data":    target,
	})
}

// @Summary Get Current Configuration
// @Description Returns the dynamic runtime configurations
// @Tags Admin
// @Accept json
// @Produce json
// @Security BearerAuth
// @Success 200 {object} map[string]interface{}
// @Router /admin/v1/config [get]
func (c *AdminController) HandleGetConfig(ctx fiber.Ctx) error {
	return ctx.JSON(fiber.Map{
		"log_raw_requests": c.cfg.Gemini.LogRawRequests,
		"auto_delete_chat": c.cfg.Gemini.AutoDeleteChat,
	})
}

type UpdateConfigRequest struct {
	LogRawRequests *bool `json:"log_raw_requests"`
	AutoDeleteChat *bool `json:"auto_delete_chat"`
}

// @Summary Update Configuration
// @Description Dynamically updates runtime configs like logging and chat deletion
// @Tags Admin
// @Accept json
// @Produce json
// @Param request body UpdateConfigRequest true "Configuration Toggles"
// @Security BearerAuth
// @Success 200 {object} map[string]interface{}
// @Router /admin/v1/config [put]
func (c *AdminController) HandleUpdateConfig(ctx fiber.Ctx) error {
	var req UpdateConfigRequest
	if err := ctx.Bind().Body(&req); err != nil {
		return ctx.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "Invalid request body"})
	}

	if req.LogRawRequests != nil {
		c.cfg.Gemini.LogRawRequests = *req.LogRawRequests
		c.log.Info("Admin Updated Config", zap.Bool("log_raw_requests", *req.LogRawRequests))
	}
	if req.AutoDeleteChat != nil {
		c.cfg.Gemini.AutoDeleteChat = *req.AutoDeleteChat
		c.log.Info("Admin Updated Config", zap.Bool("auto_delete_chat", *req.AutoDeleteChat))
	}

	return ctx.JSON(fiber.Map{
		"status":  "success",
		"message": "Configuration updated",
		"data": fiber.Map{
			"log_raw_requests": c.cfg.Gemini.LogRawRequests,
			"auto_delete_chat": c.cfg.Gemini.AutoDeleteChat,
		},
	})
}

// AuthMiddleware protects admin routes using ADMIN_API_KEY
func (c *AdminController) AuthMiddleware(ctx fiber.Ctx) error {
	expectedKey := strings.TrimSpace(c.cfg.Server.AdminAPIKey)
	
	// If no admin key is set, we still deny access for security (or we could allow it. Better to prompt user to set it)
	if expectedKey == "" {
		return ctx.Status(fiber.StatusUnauthorized).JSON(fiber.Map{
			"error": "ADMIN_API_KEY is not configured in .env. Admin operations are locked.",
		})
	}

	authHeader := ctx.Get("Authorization")
	if authHeader == "" {
		return ctx.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "Missing Authorization header"})
	}

	token := strings.TrimPrefix(authHeader, "Bearer ")
	if token != expectedKey {
		return ctx.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "Invalid API Key"})
	}

	return ctx.Next()
}

// Register registers the Admin routes
func (c *AdminController) Register(app *fiber.App) {
	admin := app.Group("/admin/v1", c.AuthMiddleware)

	admin.Get("/accounts", c.HandleListAccounts)
	admin.Post("/accounts", c.HandleAddAccount)
	admin.Delete("/accounts/:id", c.HandleDeleteAccount)
	admin.Post("/accounts/:id/test", c.HandleTestAccount)

	admin.Get("/config", c.HandleGetConfig)
	admin.Put("/config", c.HandleUpdateConfig)
}
