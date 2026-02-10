package vllm

import (
	"context"
	"testing"

	"github.com/capyflow/allspark-go/conv"
	"github.com/smartystreets/goconvey/convey"
)

func TestOllama(t *testing.T) {
	convey.Convey("TestOllama", t, func() {
		os := NewOllamaLLMService(context.TODO(), WithSelectEndpoint(func(ctx context.Context) (string, error) {
			return "http://192.168.10.205:11434", nil
		}))
		convey.So(os, convey.ShouldNotBeNil)
		models, err := os.ListLLMModels()
		convey.So(err, convey.ShouldBeNil)
		convey.So(models, convey.ShouldNotBeNil)
		t.Logf("result is %s", conv.ToJsonWithoutError(models))
	})
}

func TestOllamaChat(t *testing.T) {
	convey.Convey("TestOllamaChat", t, func() {
		os := NewOllamaLLMService(context.TODO(), WithSelectEndpoint(func(ctx context.Context) (string, error) {
			return "http://192.168.10.205:11434", nil
		}))
		convey.So(os, convey.ShouldNotBeNil)
		respChan, err := os.SendMessageStream(&LLMMessage{
			Model: "qwen3-vl:8b",
			Contents: []*LLMMessageContent{
				{
					Role:    "user",
					Content: "请介绍一下你自己",
				},
			},
		})
		convey.So(err, convey.ShouldBeNil)
		for s := range respChan {
			t.Log(s)
		}
	})
}
