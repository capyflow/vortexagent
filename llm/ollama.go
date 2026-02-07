package vllm

import (
	"context"
	"net/http"
	"time"
)

type OllamaOption func(o *OllamaLLMService)

// OllamaLLMService 是使用ollama的LLM服务
type OllamaLLMService struct {
	ctx            context.Context                           // 上下文对象，用于控制请求的生命周期
	hcli           *http.Client                              // HTTP客户端，用于发送HTTP请求
	selectEndpoint func(ctx context.Context) (string, error) // 选择服务端点的函数，返回服务端点URL
}

func NewOllamaLLMService(ctx context.Context, opts ...OllamaOption) *OllamaLLMService {
	s := &OllamaLLMService{
		ctx:  ctx,
		hcli: &http.Client{Timeout: 30 * time.Second},
		selectEndpoint: func(ctx context.Context) (string, error) {
			panic("endpoint not found")
		},
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// 列出可用的大模型列表
func (os *OllamaLLMService) ListLLMModels() ([]*LLMModel, error) {
	panic("not implemented")
}

// 给大模型发送消息并接收响应
func (os *OllamaLLMService) SendMessage(message *LLMMessage) (string, error) {
	resultChan, err := os.SendMessageStream(message)
	if err != nil {
		return "", err
	}
	var result string
	for r := range resultChan {
		result += r
	}
	return result, nil
}

// 给大模型发送消息并以流式方式接收响应
func (os *OllamaLLMService) SendMessageStream(message *LLMMessage) (<-chan string, error) {
	panic("not implemented")
}
