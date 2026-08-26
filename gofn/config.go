package main

import "net/http"

// getApiConfig mirrors the Firebase onRequest of the same name: a public config
// endpoint that returns the Joomla API key (empty string when unset).
func (s *server) handleGetApiConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"apiKey": getenv("JOOMLA_API_KEY", ""),
	})
}
