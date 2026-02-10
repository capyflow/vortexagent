package vllm

// 服务用于与大模型通信的方法
type LLMCommunicationService interface {
	ListLLMModels() ([]*LLMModel, error)                          // 列出可用的大模型列表
	SendMessage(message *LLMMessage) (string, error)              // 给大模型发送消息并接收响应
	SendMessageStream(message *LLMMessage) (<-chan string, error) // 给大模型发送消息并以流式方式接收响应
}

type LLMModel struct {
	Name          string `json:"name"`           // 模型名称
	Model         string `json:"model"`          // 模型名称
	ContextLength int64  `json:"context_length"` // 上下文长度
}

type LLMMessageContent struct {
	Role    string   `json:"role"`
	Content string   `json:"content"`
	Images  []string `json:"images"`
}

// LLMMessage 是给大模型发送的消息结构体
type LLMMessage struct {
	Contents []*LLMMessageContent `json:"contents,omitempty"` // 消息内容列表，可以包含多条消息
	Model    string               `json:"model,omitempty"`    // 使用的模型名称
	Stream   bool                 `json:"stream,omitempty"`   // 是否启用流式输出
	Think    bool                 `json:"think,omitempty"`    // 是否启用思考模式,如果为true,模型会在生成响应前先输出思考过程,如推理步骤或中间结果
	Tools    []LLMTools           `json:"tools,omitempty"`    // 使用的工具列表,如果模型支持工具调用,可以在这里指定需要使用的工具,如计算器、搜索等
}

// LLMResponse 是大模型的响应结构体
type LLMResponse struct {
	Content  string `json:"content"`  // 响应内容
	Stream   bool   `json:"stream"`   // 是否启用流式输出
	Finished bool   `json:"finished"` // 是否完成响应
}

type LLMTools struct {
	Type     string `json:"type"` // 工具类型，如"calculator"、"search"等
	Function struct {
		Name        string `json:"name"`        // 函数名称
		Description string `json:"description"` // 函数描述
		Parameters  struct {
			Type       string              `json:"type"` // 参数类型
			Properties map[string]struct { // 参数属性
				Type        string   `json:"type"`                  // 参数类型
				Description string   `json:"description,omitempty"` // 参数描述
				Enum        []string `json:"enum,omitempty"`        // 参数枚举值
			} `json:"properties"` // 参数属性
			Required []string `json:"required,omitempty"` // 必填参数
		} `json:"parameters"` // 函数参数
	} `json:"function"`
}
