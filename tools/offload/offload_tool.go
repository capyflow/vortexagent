package offload

import (
	"context"
	"fmt"

	"github.com/capyflow/vortexagent/agent"
)

type OffloadTool struct {
	manager *agent.ContextManager
}

func NewOffloadTool(manager *agent.ContextManager) *OffloadTool {
	return &OffloadTool{manager: manager}
}

func (t *OffloadTool) Name() string { return "offload_context" }

func (t *OffloadTool) Description() string {
	return `将历史对话卸载到归档存储，释放上下文空间。
当对话历史过长时，可以手动触发卸载，将旧消息压缩成摘要。`
}

func (t *OffloadTool) Schema() map[string]any {
	return map[string]any{
		"type":       "object",
		"properties": map[string]any{},
	}
}

func (t *OffloadTool) Call(ctx context.Context, args map[string]any) (string, error) {
	if t.manager == nil {
		return "", fmt.Errorf("上下文管理器未初始化")
	}

	if !t.manager.ShouldOffload() {
		return "当前上下文不需要卸载", nil
	}

	if err := t.manager.Offload(ctx); err != nil {
		return "", fmt.Errorf("卸载失败: %w", err)
	}

	return "上下文已成功卸载", nil
}

var _ agent.Tool = (*OffloadTool)(nil)
