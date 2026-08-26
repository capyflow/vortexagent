package server

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/capyflow/vortexagent/agent"
	"github.com/capyflow/vortexagent/agent/sessionstore"
)

type Server struct {
	agent      *agent.Agent
	store      sessionstore.Store
	sessions   map[string]*sessionstore.Session
	mu         sync.RWMutex
	addr       string
	httpServer *http.Server
}

type Config struct {
	Addr   string
	Agent  *agent.Agent
	Store  sessionstore.Store
}

func New(cfg Config) *Server {
	if cfg.Addr == "" {
		cfg.Addr = ":8080"
	}

	s := &Server{
		agent:    cfg.Agent,
		store:    cfg.Store,
		sessions: make(map[string]*sessionstore.Session),
		addr:     cfg.Addr,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /chat", s.handleChat)
	mux.HandleFunc("POST /chat/stream", s.handleChatStream)
	mux.HandleFunc("GET /sessions", s.handleListSessions)
	mux.HandleFunc("POST /sessions", s.handleCreateSession)
	mux.HandleFunc("DELETE /sessions/{id}", s.handleDeleteSession)
	mux.HandleFunc("GET /health", s.handleHealth)

	s.httpServer = &http.Server{
		Addr:         cfg.Addr,
		Handler:      mux,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 5 * time.Minute,
	}

	return s
}

func (s *Server) Start() error {
	return s.httpServer.ListenAndServe()
}

func (s *Server) Shutdown(ctx context.Context) error {
	return s.httpServer.Shutdown(ctx)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func (s *Server) getSession(id string) *sessionstore.Session {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.sessions[id]
}

func (s *Server) setSession(id string, sess *sessionstore.Session) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[id] = sess
}

func (s *Server) deleteSession(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, id)
}
