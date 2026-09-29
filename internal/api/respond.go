package api

import (
	"encoding/json"
	"net/http"
)

// writeJSON writes v as JSON with the given status.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if v == nil {
		return
	}
	if err := json.NewEncoder(w).Encode(v); err != nil {
		// The status line is already out; nothing useful left to do but stop.
		return
	}
}

// errorBody is the shape of every error response.
type errorBody struct {
	Error string `json:"error"`
}

// writeError reports a failure in the standard envelope.
func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, errorBody{Error: message})
}
