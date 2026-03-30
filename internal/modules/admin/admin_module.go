package admin

import (
	"github.com/gofiber/fiber/v3"
	"go.uber.org/fx"
)

var Module = fx.Options(
	fx.Provide(NewAdminController),
	fx.Invoke(RegisterRoutes),
)

func RegisterRoutes(app *fiber.App, c *AdminController) {
	c.Register(app)
}
