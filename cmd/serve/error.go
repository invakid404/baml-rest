package main

import (
	"net/http"

	fiberrequestid "github.com/gofiber/fiber/v3/middleware/requestid"
	"github.com/invakid404/baml-rest/internal/apierror"
)

// writeJSONError writes a JSON-formatted error response with the given status code.
func writeJSONError(w http.ResponseWriter, r *http.Request, message string, statusCode int) {
	apierror.WriteJSON(w, message, statusCode, fiberrequestid.FromContext(r.Context()))
}
