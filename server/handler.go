package server

import (
	"encoding/json"
	"net/http"

	"github.com/capyflow/vortexagent/agent/sessionstore"
)

type ChatRequest struct {
	SessionID string `json:"session_id"`
	Message   string `json:"message"`
}

type ChatResponse struct {
	SessionID string `json:"session_id"`
	Answer    string `json:"answer"`
	Error     string `json:"error,omitempty"`
}

type SessionResponse struct {
	ID        string `json:"id"`
	Model     string `json:"model"`
	Messages  int    `json:"messages"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	var req ChatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, ChatResponse{Error: "无效的请求格式"})
		return
	}

	if req.Message == "" {
		writeJSON(w, http.StatusBadRequest, ChatResponse{Error: "message 不能为空"})
		return
	}

	sess := s.getOrCreateSession(req.SessionID)

	answer, err := s.agent.Ask(r.Context(), sess, req.Message)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, ChatResponse{
			SessionID: sess.ID,
			Error:     err.Error(),
		})
		return
	}

	writeJSON(w, http.StatusOK, ChatResponse{
		SessionID: sess.ID,
		Answer:    answer,
	})
}

func (s *Server) handleListSessions(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	sessions := make([]SessionResponse, 0, len(s.sessions))
	for _, sess := range s.sessions {
		sessions = append(sessions, SessionResponse{
			ID:        sess.ID,
			Model:     sess.Model,
			Messages:  len(sess.History),
			CreatedAt: sess.CreatedAt.Format("2006-01-02 15:04:05"),
			UpdatedAt: sess.UpdatedAt.Format("2006-01-02 15:04:05"),
		})
	}

	writeJSON(w, http.StatusOK, sessions)
}

func (s *Server) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Model string `json:"model"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		req.Model = "default"
	}

	sess := sessionstore.NewSession(req.Model)
	s.setSession(sess.ID, sess)

	writeJSON(w, http.StatusCreated, SessionResponse{
		ID:        sess.ID,
		Model:     sess.Model,
		Messages:  0,
		CreatedAt: sess.CreatedAt.Format("2006-01-02 15:04:05"),
		UpdatedAt: sess.UpdatedAt.Format("2006-01-02 15:04:05"),
	})
}

func (s *Server) handleDeleteSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "id 不能为空"})
		return
	}

	s.deleteSession(id)
	writeJSON(w, http.StatusOK, map[string]string{"message": "已删除"})
}

func (s *Server) getOrCreateSession(id string) *sessionstore.Session {
	if id != "" {
		if sess := s.getSession(id); sess != nil {
			return sess
		}
	}

	sess := sessionstore.NewSession("default")
	s.setSession(sess.ID, sess)
	return sess
}

func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}
