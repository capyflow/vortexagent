package agent

import (
	"context"
	"fmt"
	"testing"

	"github.com/capyflow/vortexagent/agent/sessionstore"
	"github.com/capyflow/vortexagent/llm"
	timetool "github.com/capyflow/vortexagent/tools/time"
)

// 测试问题
func Test_Ask(t *testing.T) {

	ctx := context.Background()

	provider := llm.NewOpenAIProvider(
		"Fz9ZzxdlPkq8TwNCvZm9qdA7ZFCq9wo4QkgJ37PqTjdFX8NT65XcLSwz9oHKMqBS",
		llm.WithBaseURL("https://api.stepfun.com/step_plan/v1"),
		llm.WithModel("step-3.7-flash"))

	agent := New(Options{
		Provider: provider,
	})

	answer, err := agent.Ask(ctx, &sessionstore.Session{}, "今天是几月几日")
	if nil != err {
		fmt.Printf("Ask|Error|%v", err)
	} else {
		fmt.Printf("Ask|Result|%s", answer)
	}
}

func Test_AskWithTools(t *testing.T) {

	ctx := context.Background()

	provider := llm.NewOpenAIProvider(
		"Fz9ZzxdlPkq8TwNCvZm9qdA7ZFCq9wo4QkgJ37PqTjdFX8NT65XcLSwz9oHKMqBS",
		llm.WithBaseURL("https://api.stepfun.com/step_plan/v1"),
		llm.WithModel("step-3.7-flash"))

	registry := NewRegistry()
	registry.Add(&timetool.TimeTool{})

	agent := New(Options{
		Provider: provider,
		Registry: registry,
	})

	answer, err := agent.Ask(ctx, &sessionstore.Session{}, "今天是几月几日")
	if nil != err {
		fmt.Printf("Ask|Error|%v", err)
	} else {
		fmt.Printf("Ask|Result|%s", answer)
	}
}
