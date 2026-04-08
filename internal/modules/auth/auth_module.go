package auth

import (
	"github.com/gofiber/fiber/v3"
	"go.uber.org/fx"
)

var Module = fx.Options(
	fx.Provide(NewAuthController),
	fx.Invoke(RegisterRoutes),
)

func RegisterRoutes(app *fiber.App, c *AuthController) {
	c.Register(app)
}
