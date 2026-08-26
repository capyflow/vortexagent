package server

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/capyflow/vortexagent/llm"
)

type StreamEvent struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

func (s *Server) handleChatStream(w http.ResponseWriter, r *http.Request) {
	var req ChatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "无效的请求格式", http.StatusBadRequest)
		return
	}

	if req.Message == "" {
		http.Error(w, "message 不能为空", http.StatusBadRequest)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "不支持 SSE", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	sess := s.getOrCreateSession(req.SessionID)

	sendEvent := func(event StreamEvent) error {
		data, _ := json.Marshal(event)
		fmt.Fprintf(w, "data: %s\n\n", data)
		flusher.Flush()
		return nil
	}

	var answer string
	var answerErr error

	onDelta := func(d llm.Delta) {
		if d.Text != "" {
			sendEvent(StreamEvent{Type: "delta", Text: d.Text})
		}
	}

	ag := s.agent.WithDelta(onDelta)
	answer, answerErr = ag.Ask(r.Context(), sess, req.Message)

	if answerErr != nil {
		sendEvent(StreamEvent{Type: "error", Text: answerErr.Error()})
	} else {
		sendEvent(StreamEvent{Type: "done", Text: answer})
	}
}
