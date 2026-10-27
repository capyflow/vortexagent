// Package auth 实现 MCP 客户端的 OAuth 2.1 授权流程（MCP 2025-06-18 授权规范）：
// 从 401 / WWW-Authenticate 或 well-known 发现授权服务器，按需动态注册客户端，
// 走 PKCE 授权码 + 本地 loopback 回调，最后交换/刷新 token。
//
// 包本身不依赖 MCP transport、不绑定任何产品：调用方负责发 MCP 请求、决定 token
// 存到哪里，以及把授权 URL 打开到浏览器。Notion 等具体服务只通过元数据进入，
// 代码里没有服务商分支。
package auth
