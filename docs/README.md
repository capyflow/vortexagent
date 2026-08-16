# Vortex 文档

按你的需求选：

| 文档 | 适合谁 | 内容 |
|------|--------|------|
| [tutorial.md](tutorial.md) | **初学者**（没写过 agent） | 从零动手：10 节课搭起自己的 agent，每课可运行、可练习 |
| [architecture.md](architecture.md) | 想理解设计原理的人 | 框架分层（核心 / 扩展 / 应用）、每个包的职责、关键设计决策与踩坑记录 |
| [../README.md](../README.md) | 所有使用者 | 快速开始、配置参考、扩展指南、测试策略 |

**建议路径**：先按 [tutorial.md](tutorial.md) 动手跑通 → 再读
[architecture.md](architecture.md) 理解为什么这么设计 → 最后回到 README 看完整能力清单。

## 代码地图

```
llm/              统一 LLM 协议 + 多厂商适配器（openai / anthropic / gemini）
agent/            Agent 运行时：循环（ask.go）、工具（tool.go）、会话（session.go）、
                  钩子（hooks.go）、存储（store.go）
tools/mcp/        MCP 客户端：接入任意语言编写的自定义工具
tools/knowledge/  知识库扩展：文档检索工具（可选）
cmd/vortex/       参考 CLI：通用 REPL agent（应用层示例）
examples/         用法示例：minimal-agent / knowledge-agent / mcp-server
```
