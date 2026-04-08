package admin

import (
	"context"
	"fmt"
	"gemini-web-to-api/internal/commons/configs"
	"gemini-web-to-api/internal/modules/providers"
	"strings"
	"time"

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

	if strings.HasPrefix(id, "guest-") {
		platform := strings.TrimPrefix(id, "guest-")
		c.client.RemoveAccount(id)
		return ctx.JSON(fiber.Map{"status": "success", "message": fmt.Sprintf("Guest platform %s removed", platform)})
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

type ConfigResponse struct {
	LogRawRequests       bool `json:"log_raw_requests"`
	AutoDeleteChat       bool `json:"auto_delete_chat"`
	EnableGuestDiscovery bool `json:"enable_guest_discovery"`
}

// @Summary Get Current Configuration
// @Description Returns the dynamic runtime configurations
// @Tags Admin
// @Accept json
// @Produce json
// @Security BearerAuth
// @Success 200 {object} ConfigResponse
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

type UpdateConfigResponse struct {
	Status  string         `json:"status"`
	Message string         `json:"message"`
	Data    ConfigResponse `json:"data"`
}

// @Summary Update Configuration
// @Description Dynamically updates runtime configs like logging and chat deletion
// @Tags Admin
// @Accept json
// @Produce json
// @Param request body UpdateConfigRequest true "Configuration Toggles"
// @Security BearerAuth
// @Success 200 {object} UpdateConfigResponse
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

	// Guest Management
	admin.Post("/guest/discover", c.HandleTriggerDiscovery)
	admin.Get("/guest/platforms", c.HandleListGuestPlatforms)
	admin.Delete("/guest/platforms/:id", c.HandleDeleteGuestPlatform)
	admin.Post("/guest/platforms/:id/test", c.HandleTestGuestPlatform)
	admin.Post("/guest/platforms/:id/enable", c.HandleEnableGuestPlatform)
	admin.Post("/guest/platforms/:id/disable", c.HandleDisableGuestPlatform)
	admin.Post("/guest/relearn", c.HandleRelearnGuest)
	admin.Post("/guest/validate-raw", c.HandleValidateGuestsByRawCall) // NEW: bypass is_valid, test via actual HTTP

	// Self-Healing
	admin.Post("/schema/reset", c.HandleResetSchema)
	admin.Post("/schema/retry", c.HandleRetrySchema)
	admin.Get("/schema/status", c.HandleGetSchemaStatus)

	// Anti-Bot
	admin.Post("/accounts/:id/clear-bot", c.HandleClearBot)
	admin.Post("/accounts/clear-all-bots", c.HandleClearAllBots)
}

// --- Guest Handlers ---

// @Summary Trigger Guest Discovery
// @Description Manually triggers the discovery service to find and learn new AI guest platforms
// @Tags Guest
// @Security BearerAuth
// @Router /admin/v1/guest/discover [post]
func (c *AdminController) HandleTriggerDiscovery(ctx fiber.Ctx) error {
	// No autonomous discovery to toggle
	return ctx.Status(fiber.StatusOK).JSON(fiber.Map{
		"status":  "ok",
		"enabled": false,
	})
}

// @Summary List Guest Platforms
// @Description Returns the list of discovered and valid guest chat platforms
// @Tags Guest
// @Security BearerAuth
// @Router /admin/v1/guest/platforms [get]
func (c *AdminController) HandleListGuestPlatforms(ctx fiber.Ctx) error {
	platforms := c.client.GetMultiGuestMgr().GetConfigs()
	return ctx.JSON(fiber.Map{"data": platforms})
}

// @Summary Delete Guest Platform
// @Description Removes a discovered guest platform from the system
// @Tags Guest
// @Param id path string true "Platform ID"
// @Security BearerAuth
// @Router /admin/v1/guest/platforms/{id} [delete]
func (c *AdminController) HandleDeleteGuestPlatform(ctx fiber.Ctx) error {
	id := ctx.Params("id")
	if id == "" {
		return ctx.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "Missing platform ID"})
	}
	
	// Ensure it has guest- prefix for RemoveAccount logic
	fullID := "guest-" + strings.TrimPrefix(id, "guest-")
	c.client.RemoveAccount(fullID)
	return ctx.JSON(fiber.Map{"status": "success", "message": fmt.Sprintf("Guest platform %s removed", id)})
}

// @Summary Test Guest Platform
// @Description Send 'hello' to a guest platform to verify it's working
// @Tags Guest
// @Param id path string true "Platform ID"
// @Security BearerAuth
// @Router /admin/v1/guest/platforms/{id}/test [post]
func (c *AdminController) HandleTestGuestPlatform(ctx fiber.Ctx) error {
	id := ctx.Params("id")
	if id == "" {
		return ctx.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "Missing platform ID"})
	}
	
	platform := strings.TrimPrefix(id, "guest-")
	err := c.client.TestGuest(platform)
	if err != nil {
		return ctx.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"status": "error",
			"error":  err.Error(),
			"message": "Kiểm tra thấy lỗi. Quá trình lấy cấu hình mới đã được kích hoạt.",
		})
	}

	return ctx.JSON(fiber.Map{"status": "success", "message": "Guest hoạt động tốt!"})
}

// @Summary Disable Guest Platform
// @Description Manually disable a discovered guest platform
// @Tags Guest
// @Param id path string true "Platform ID"
// @Security BearerAuth
// @Router /admin/v1/guest/platforms/{id}/disable [post]
func (c *AdminController) HandleDisableGuestPlatform(ctx fiber.Ctx) error {
	id := ctx.Params("id")
	if id == "" {
		return ctx.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "Missing platform ID"})
	}

	platform := strings.TrimPrefix(id, "guest-")
	c.client.SetGuestDisabled(platform, true)

	return ctx.JSON(fiber.Map{
		"status":  "success",
		"message": fmt.Sprintf("Guest platform %s disabled successfully", platform),
	})
}

// @Summary Enable Guest Platform
// @Description Manually enable a discovered guest platform
// @Tags Guest
// @Param id path string true "Platform ID"
// @Security BearerAuth
// @Router /admin/v1/guest/platforms/{id}/enable [post]
func (c *AdminController) HandleEnableGuestPlatform(ctx fiber.Ctx) error {
	id := ctx.Params("id")
	if id == "" {
		return ctx.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "Missing platform ID"})
	}

	platform := strings.TrimPrefix(id, "guest-")
	c.client.SetGuestDisabled(platform, false)

	return ctx.JSON(fiber.Map{
		"status":  "success",
		"message": fmt.Sprintf("Guest platform %s enabled successfully", platform),
	})
}

// @Summary Re-learn Guest Platforms
// @Description Resets learned structure for guest platforms and initiates re-learning
// @Tags Guest
// @Param id query string false "Specific Platform ID (optional, omit for ALL)"
// @Security BearerAuth
// @Router /admin/v1/guest/relearn [post]
func (c *AdminController) HandleRelearnGuest(ctx fiber.Ctx) error {
	id := ctx.Query("id")
	if id != "" {
		platform := strings.TrimPrefix(id, "guest-")
		c.client.ResetGuest(platform)
		return ctx.JSON(fiber.Map{"status": "pending", "message": fmt.Sprintf("Re-learning initiated for guest: %s", platform)})
	}

	c.client.ResetAllGuests()
	return ctx.JSON(fiber.Map{"status": "pending", "message": "Re-learning initiated for ALL guest platforms"})
}

// @Summary Validate Guest Platforms by Raw HTTP Call
// @Description Bypasses is_valid flag and directly calls each platform's API to test if schema works.
// @Description Sets is_valid=true for platforms that respond correctly.
// @Tags Guest
// @Security BearerAuth
// @Router /admin/v1/guest/validate-raw [post]
func (c *AdminController) HandleValidateGuestsByRawCall(ctx fiber.Ctx) error {
	c.log.Info("🔬 Admin: Triggering raw-call validation for all guest platforms")

	results := c.client.ValidateAllGuestsByRawCall(ctx.Context())

	validCount := 0
	for _, r := range results {
		if r.IsValid {
			validCount++
		}
	}

	return ctx.JSON(fiber.Map{
		"status":      "completed",
		"total":       len(results),
		"valid_count": validCount,
		"results":     results,
		"message":     fmt.Sprintf("%d/%d platforms validated successfully and set to is_valid=true", validCount, len(results)),
	})
}

// @Summary Reset Schema Extractor
// @Description Resets the GJSON extraction paths to default and triggers healing
// @Tags Admin
// @Security BearerAuth
// @Router /admin/v1/schema/reset [post]
func (c *AdminController) HandleResetSchema(ctx fiber.Ctx) error {
	c.client.GetSchemaMgr().Reset()
	go func() {
		// Run healing in background
		c.client.RunHealing(context.Background())
	}()
	return ctx.JSON(fiber.Map{"status": "success", "message": "Schema reset and healing triggered"})
}

// @Summary Retry Healing
// @Description Manually triggers the self-healing process to find new GJSON paths
// @Tags Admin
// @Security BearerAuth
// @Router /admin/v1/schema/retry [post]
func (c *AdminController) HandleRetrySchema(ctx fiber.Ctx) error {
	if c.client.IsHealing() {
		return ctx.Status(fiber.StatusConflict).JSON(fiber.Map{"error": "Healing is already in progress"})
	}
	go func() {
		c.client.RunHealing(context.Background())
	}()
	return ctx.JSON(fiber.Map{"status": "success", "message": "Healing process triggered"})
}

// @Summary Get Schema Status
// @Description Returns the current GJSON schema and healing status
// @Tags Admin
// @Security BearerAuth
// @Router /admin/v1/schema/status [get]
func (c *AdminController) HandleGetSchemaStatus(ctx fiber.Ctx) error {
	isHealing := c.client.IsHealing()
	statusText := c.client.GetHealingStatus()
	
	return ctx.JSON(fiber.Map{
		"is_healing":  isHealing,
		"status_text": statusText,
		"schema":      c.client.GetSchemaMgr().GetSchema(),
		"message":     fmt.Sprintf("Hệ thống: %s", statusText),
	})
}

// @Summary Clear Bot for specific account
// @Description Manually triggers the Rod browser to perform "hãy trả lời ok" interaction
// @Tags Admin
// @Param id path string true "Account ID"
// @Security BearerAuth
// @Router /admin/v1/accounts/{id}/clear-bot [post]
func (c *AdminController) HandleClearBot(ctx fiber.Ctx) error {
	id := ctx.Params("id")
	go func() {
		clearCtx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()
		if err := c.client.ClearBot(clearCtx, id); err != nil {
			c.log.Error("❌ Admin ClearBot FAILED", zap.String("id", id), zap.Error(err))
		}
	}()
	return ctx.JSON(fiber.Map{"status": "pending", "message": fmt.Sprintf("Bot clearance initiated for %s", id)})
}

// @Summary Clear All Bots
// @Description Sweeps all accounts and performs bot clearance on those with errors
// @Tags Admin
// @Security BearerAuth
// @Router /admin/v1/accounts/clear-all-bots [post]
func (c *AdminController) HandleClearAllBots(ctx fiber.Ctx) error {
	accounts := c.client.GetAccounts()
	count := 0
	for _, acc := range accounts {
		id := acc.ID
		go func(aid string) {
			clearCtx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
			defer cancel()
			if err := c.client.ClearBot(clearCtx, aid); err != nil {
				c.log.Error("❌ Admin ClearAllBots FAILED", zap.String("id", aid), zap.Error(err))
			}
		}(id)
		count++
	}
	return ctx.JSON(fiber.Map{"status": "pending", "message": fmt.Sprintf("Bot clearance initiated for %d accounts", count)})
}
