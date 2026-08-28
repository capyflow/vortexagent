package server

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/capyflow/vortexagent/agent"
	"github.com/capyflow/vortexagent/agent/sessionstore"
)

type Server struct {
	agent    *agent.Agent
	store    sessionstore.Store
	sessions map[string]*sessionstore.Session
	mu       sync.RWMutex
	// sessionLocks 按会话 ID 串行化请求：同一会话并发 Ask 会交错写入历史、
	// 污染对话上下文。不同会话互不影响。
	sessionLocks sync.Map // sessionID → *sync.Mutex
	addr         string
	httpServer   *http.Server
}

type Config struct {
	Addr  string
	Agent *agent.Agent
	Store sessionstore.Store // 可选：配置后会话持久化，重启后按 session_id 恢复
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

// lockFor 返回会话的请求互斥锁（懒创建，LoadOrStore 保证并发下唯一）。
func (s *Server) lockFor(id string) *sync.Mutex {
	if v, ok := s.sessionLocks.Load(id); ok {
		return v.(*sync.Mutex)
	}
	v, _ := s.sessionLocks.LoadOrStore(id, &sync.Mutex{})
	return v.(*sync.Mutex)
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
	s.sessionLocks.Delete(id)
}

// saveSession 把会话写入持久化存储（未配置 Store 时为空操作）。
// 失败只记日志不中断请求：持久化是尽力而为，不该让一次成功回答变 500。
func (s *Server) saveSession(sess *sessionstore.Session) {
	if s.store == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.store.Save(ctx, sess); err != nil {
		log.Printf("[vortex-server] 保存会话 %s 失败: %v", sess.ID, err)
	}
}
