package vllm

// 服务用于与大模型通信的方法
type LLMCommunicationService interface {
	ListLLMModels() ([]*LLMModel, error)                          // 列出可用的大模型列表
	SendMessage(message *LLMMessage) (string, error)              // 给大模型发送消息并接收响应
	SendMessageStream(message *LLMMessage) (<-chan string, error) // 给大模型发送消息并以流式方式接收响应
}

type LLMModel struct {
	Name string `json:"name"` // 模型名称
}

// LLMMessage 是给大模型发送的消息结构体
type LLMMessage struct {
	Content     string   `json:"content"`     // 消息内容
	Prompt      string   `json:"prompt"`      // 提示语
	Role        string   `json:"role"`        // 消息角色，如 "user" 或 "assistant"
	Model       string   `json:"model"`       // 使用的模型名称
	Stream      bool     `json:"stream"`      // 是否启用流式输出
	Attachments []string `json:"attachments"` // 附件文件列表,如图片或文档,可以是URL或Base64编码的字符串
}

// LLMResponse 是大模型的响应结构体
type LLMResponse struct {
	Content  string `json:"content"`  // 响应内容
	Stream   bool   `json:"stream"`   // 是否启用流式输出
	Finished bool   `json:"finished"` // 是否完成响应
}
