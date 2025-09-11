package main

import (
	"github.com/gofiber/fiber/v2"
)

func main() {
	app := fiber.New()

	app.Get("/", func (c *fiber.Ctx) error {
		return c.SendString("home")
	})

	app.Get("/login", func (c *fiber.Ctx) error {
		return c.SendString("login")
	})

	app.Post("/form_upload", func (c *fiber.Ctx) error {
		return c.SendString("form_upload")
	})

	app.Get("/image", func (c *fiber.Ctx) error {
		return c.SendString("image")
	})

	app.Get("/list_all_participants", func (c *fiber.Ctx) error {
		return c.SendString("list_all_participants")
	})


}
