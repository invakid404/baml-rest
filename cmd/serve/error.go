package main

import (
	"github.com/gofiber/fiber/v3"
	fiberrequestid "github.com/gofiber/fiber/v3/middleware/requestid"
	"github.com/invakid404/baml-rest/internal/apierror"
)

// writeFiberJSONError writes a JSON-formatted error response for native Fiber handlers.
func writeFiberJSONError(c fiber.Ctx, message string, statusCode int) error {
	return c.Status(statusCode).JSON(apierror.Response{
		Error:     message,
		RequestID: fiberrequestid.FromContext(c),
	})
}
