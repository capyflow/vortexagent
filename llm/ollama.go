package vllm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/capyflow/allspark-go/logx"
	"github.com/capyflow/vortexagent/pkg"
)

func init() {
	logx.WithEnableFile(false)
	logx.ResetLogger()
}

const (
	listModels = "/api/ps"
	modelChat  = "/api/chat"
)

type OllamaOption func(o *OllamaLLMService)

// OllamaLLMService 是使用ollama的LLM服务
type OllamaLLMService struct {
	ctx            context.Context                           // 上下文对象，用于控制请求的生命周期
	hcli           *http.Client                              // HTTP客户端，用于发送HTTP请求
	selectEndpoint func(ctx context.Context) (string, error) // 选择服务端点的函数，返回服务端点URL
	streamInterval time.Duration                             // 流式返回的频率
}

// WithSelectEndpoint 设置服务端点的选项
func WithSelectEndpoint(selectEndpoint func(ctx context.Context) (string, error)) OllamaOption {
	return func(o *OllamaLLMService) {
		o.selectEndpoint = selectEndpoint
	}
}

// WithStreamInterval 设置流式返回的频率的选项
func WithStreamInterval(interval time.Duration) OllamaOption {
	return func(o *OllamaLLMService) {
		o.streamInterval = interval
	}
}

// WithHttpClient 设置HTTP客户端的选项
func WithHttpClient(hcli *http.Client) OllamaOption {
	return func(o *OllamaLLMService) {
		o.hcli = hcli
	}
}

func NewOllamaLLMService(ctx context.Context, opts ...OllamaOption) *OllamaLLMService {
	s := &OllamaLLMService{
		ctx:  ctx,
		hcli: &http.Client{Timeout: 10 * time.Minute},
		selectEndpoint: func(ctx context.Context) (string, error) {
			panic("endpoint not found")
		},
		streamInterval: 500 * time.Millisecond,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

type ollamaStreamResponse struct {
	Model     string `json:"model"`
	CreatedAt string `json:"created_at"`
	Message   struct {
		Role     string `json:"role"`
		Content  string `json:"content"`
		Thinking string `json:"thinking"`
	} `json:"message"`
	Done bool `json:"done"`
}

// 列出可用的大模型列表
func (os *OllamaLLMService) ListLLMModels() ([]*LLMModel, error) {
	ollamaEndpoint, err := os.selectEndpoint(os.ctx)
	if nil != err {
		return nil, err
	}
	url := ollamaEndpoint + listModels
	fmt.Println(url)
	resp, err := os.hcli.Get(url)
	if nil != err {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, err
	}
	var result struct {
		Models []*LLMModel `json:"models"`
	}

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	err = json.Unmarshal(raw, &result)
	if err != nil {
		return nil, err
	}
	if len(result.Models) == 0 {
		return nil, pkg.ErrorsEnums.ErrModelsNotFound
	}
	return result.Models, nil
}

// 给大模型发送消息并接收响应
func (os *OllamaLLMService) SendMessage(message *LLMMessage) (*LLMResponse, error) {
	resultChan, err := os.SendMessageStream(message)
	if err != nil {
		return nil, err
	}
	resp := &LLMResponse{Stream: false, Finished: true}
	for r := range resultChan {
		var rsp LLMResponse
		if err := json.Unmarshal([]byte(r), &rsp); err != nil {
			logx.Errorf("OllamaLLMService|SendMessage|unmarshal stream error: %v", err)
			continue
		}
		if message.Think && len(rsp.Think) > 0 {
			resp.Think += rsp.Think
		} else if len(rsp.Content) > 0 {
			resp.Content += rsp.Content
		}
		if rsp.Finished {
			break
		}
	}
	return resp, nil
}

// 给大模型发送消息并以流式方式接收响应
func (os *OllamaLLMService) SendMessageStream(message *LLMMessage) (<-chan string, error) {
	message.Stream = true
	endpoint, err := os.selectEndpoint(os.ctx)
	if nil != err {
		logx.Errorf("OllamaLLMService|SendMessageStream|select endpoint error: %v", err)
		return nil, err
	}
	url := endpoint + modelChat
	raw, err := json.Marshal(message)
	if nil != err {
		logx.Errorf("OllamaLLMService|SendMessageStream|marshal message error: %v", err)
		return nil, err
	}

	logx.Debugf("OllamaLLMService|SendMessageStream|request body: %s", string(raw))

	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(raw))
	if nil != err {
		logx.Errorf("OllamaLLMService|SendMessageStream|new request error: %v", err)
		return nil, err
	}
	resp, err := os.hcli.Do(req)
	if nil != err {
		logx.Errorf("OllamaLLMService|SendMessageStream|do request error: %v", err)
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		rawErr, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		return nil, fmt.Errorf("ollama request failed: status=%d body=%s", resp.StatusCode, string(rawErr))
	}

	resultChan := make(chan string)
	go func() {
		defer close(resultChan)
		defer resp.Body.Close()
		reader := bufio.NewReader(resp.Body)
		for {
			line, readErr := reader.ReadBytes('\n')
			if len(line) > 0 {
				lineStr := strings.TrimSpace(string(line))
				var ollamaResp ollamaStreamResponse
				if err := json.Unmarshal([]byte(lineStr), &ollamaResp); err != nil {
					logx.Errorf("OllamaLLMService|SendMessageStream|unmarshal stream error: %v", err)
					continue
				}
				llmResp := LLMResponse{
					Finished: ollamaResp.Done,
					Stream:   true,
				}
				if message.Think && len(ollamaResp.Message.Thinking) > 0 {
					llmResp.Think = ollamaResp.Message.Thinking
				} else if len(ollamaResp.Message.Content) > 0 {
					llmResp.Content = ollamaResp.Message.Content
				}

				marshal, _ := json.Marshal(llmResp)
				resultChan <- string(marshal)
			}
			if readErr != nil {
				if readErr != io.EOF {
					logx.Errorf("OllamaLLMService|SendMessageStream|read stream error: %v", readErr)
				}
				break
			}
		}
	}()
	return resultChan, nil
}
