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
		hcli: &http.Client{Timeout: 30 * time.Second},
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

type OllamaResponse struct {
	Model              string    `json:"model"`
	CreatedAt          time.Time `json:"created_at"`
	Message            Message   `json:"message"`
	DoneReason         string    `json:"done_reason"`
	Done               bool      `json:"done"`
	TotalDuration      int       `json:"total_duration"`
	LoadDuration       int       `json:"load_duration"`
	PromptEvalCount    int       `json:"prompt_eval_count"`
	PromptEvalDuration int       `json:"prompt_eval_duration"`
	EvalCount          int       `json:"eval_count"`
	EvalDuration       int       `json:"eval_duration"`
}

type Message struct {
	Role      string `json:"role"`
	Content   string `json:"content"`
	ToolCalls []struct {
		Function struct {
			Name      string `json:"name"`
			Arguments struct {
				Format   string `json:"format"`
				Location string `json:"location"`
			} `json:"arguments"`
		} `json:"function"`
	} `json:"tool_calls"`
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

	respRaw, err := io.ReadAll(resp.Body)
	if nil != err {
		logx.Errorf("OllamaLLMService|SendMessageStream|read response error: %v", err)
		return nil, err
	}

	logx.Infof("OllamaLLMService|SendMessageStream|read response %s", string(respRaw))

	resultChan := make(chan string)
	go func() {
		defer close(resultChan)
		defer resp.Body.Close()
		reader := bufio.NewReader(bytes.NewReader(respRaw))
		for {
			line, readErr := reader.ReadBytes('\n')
			if len(line) > 0 {
				rawLine := strings.TrimSpace(string(line))
				logx.Debugf("OllamaLLMService|SendMessageStream|raw line: %s", rawLine)
				var ollamaResp OllamaResponse
				err := json.Unmarshal([]byte(rawLine), &ollamaResp)
				if err != nil {
					logx.Errorf("OllamaLLMService|SendMessageStream|unmarshal error: %v", err)
					continue
				}
				if ollamaResp.Message.Content != "" {
					resultChan <- ollamaResp.Message.Content
				}
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
