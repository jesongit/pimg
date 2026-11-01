package server

import "net/http"

func (s *Server) handleDiagnosticTelegram(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.Svc.TelegramDiagnostics())
}
