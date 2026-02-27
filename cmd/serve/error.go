package main

import (
	"net/http"

	"github.com/gofiber/fiber/v3"
	fiberrequestid "github.com/gofiber/fiber/v3/middleware/requestid"
	"github.com/invakid404/baml-rest/internal/apierror"
)

// writeJSONError writes a JSON-formatted error response with the given status code.
func writeJSONError(w http.ResponseWriter, r *http.Request, message string, statusCode int) {
	apierror.WriteJSON(w, message, statusCode, fiberrequestid.FromContext(r.Context()))
}

// writeFiberJSONError writes a JSON-formatted error response for native Fiber handlers.
func writeFiberJSONError(c fiber.Ctx, message string, statusCode int) error {
	return c.Status(statusCode).JSON(apierror.Response{
		Error:     message,
		RequestID: fiberrequestid.FromContext(c),
	})
}
