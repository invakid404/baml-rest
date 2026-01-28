// Package apierror provides utilities for returning consistent JSON error responses.
package apierror

import (
	"net/http"

	"github.com/goccy/go-json"
)

// Response is the standard JSON error response format.
type Response struct {
	Error     string `json:"error"`
	RequestID string `json:"request_id,omitempty"`
}

// WriteJSON writes a JSON-formatted error response with the given status code.
// The requestID parameter is optional and will be omitted from the response if empty.
func WriteJSON(w http.ResponseWriter, message string, statusCode int, requestID string) {
	resp := Response{
		Error:     message,
		RequestID: requestID,
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)

	// Best effort - if encoding fails, we've already written the status code
	_ = json.NewEncoder(w).Encode(resp)
}
