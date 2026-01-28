package main

import (
	"net/http"

	"github.com/go-chi/chi/v5/middleware"
	"github.com/invakid404/baml-rest/internal/apierror"
)

// writeJSONError writes a JSON-formatted error response with the given status code.
func writeJSONError(w http.ResponseWriter, r *http.Request, message string, statusCode int) {
	apierror.WriteJSON(w, message, statusCode, middleware.GetReqID(r.Context()))
}
