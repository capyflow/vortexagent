package server

import (
	"context"
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

	// 同一会话串行化：并发 Ask 会交错追加历史、污染上下文。
	mu := s.lockFor(sess.ID)
	mu.Lock()
	defer mu.Unlock()

	answer, err := s.agent.Ask(r.Context(), sess, req.Message)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, ChatResponse{
			SessionID: sess.ID,
			Error:     err.Error(),
		})
		return
	}

	s.saveSession(sess)

	writeJSON(w, http.StatusOK, ChatResponse{
		SessionID: sess.ID,
		Answer:    answer,
	})
}

func (s *Server) handleListSessions(w http.ResponseWriter, r *http.Request) {
	sessions := make([]SessionResponse, 0)

	if s.store != nil {
		// 配置了持久化时以存储为准（含其他实例写入的会话）。
		stored, err := s.store.List(r.Context())
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		for _, sess := range stored {
			sessions = append(sessions, toSessionResponse(sess, sess.MessageCount()))
		}
		writeJSON(w, http.StatusOK, sessions)
		return
	}

	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, sess := range s.sessions {
		sessions = append(sessions, toSessionResponse(sess, sess.MessageCount()))
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
	s.saveSession(sess)

	writeJSON(w, http.StatusCreated, toSessionResponse(sess, 0))
}

func (s *Server) handleDeleteSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "id 不能为空"})
		return
	}

	s.deleteSession(id)
	if s.store != nil {
		if err := s.store.Delete(context.Background(), id); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"message": "已删除"})
}

// getOrCreateSession 返回内存中的会话；未命中且配置了 Store 时从存储恢复，
// 否则创建新会话。
func (s *Server) getOrCreateSession(id string) *sessionstore.Session {
	if id != "" {
		if sess := s.getSession(id); sess != nil {
			return sess
		}
		if s.store != nil {
			loaded, err := s.store.Load(context.Background(), id)
			if err == nil && loaded != nil {
				s.setSession(loaded.ID, loaded)
				return loaded
			}
		}
	}

	sess := sessionstore.NewSession("default")
	s.setSession(sess.ID, sess)
	s.saveSession(sess)
	return sess
}

func toSessionResponse(sess *sessionstore.Session, messageCount int) SessionResponse {
	return SessionResponse{
		ID:        sess.ID,
		Model:     sess.Model,
		Messages:  messageCount,
		CreatedAt: sess.CreatedAt.Format("2006-01-02 15:04:05"),
		UpdatedAt: sess.UpdatedAt.Format("2006-01-02 15:04:05"),
	}
}

func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}
