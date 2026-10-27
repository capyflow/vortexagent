# MCP OAuth 客户端引擎

`tools/mcp/auth` 按 MCP 2025-06-18 授权规范实现客户端侧 OAuth：元数据发现、动态客户端注册（DCR）、PKCE S256、loopback 回调、token 交换与刷新。它是纯协议层，不依赖 MCP transport、不关心 token 存在哪、也不负责打开浏览器。

## 调用顺序

1. `ParseChallenge` 解析 MCP 端点 401 的 `WWW-Authenticate`（拿 `resource_metadata`）。
2. `Discover` 读受保护资源元数据与授权服务器元数据；401 里没有 URL 时自动走 well-known。
3. `Register` 做 DCR；服务商不支持 DCR 时返回 `RegistrationError`，带出需要加白的回调地址。手工 client 通过 `RegisterOptions.ClientID/ClientSecret` 直接传入。
4. `Begin` 起 loopback、注册 client、生成 state/PKCE，返回 `Flow`；调用方把 `Flow.URL()` 打开到系统浏览器。
5. `Flow.Wait` 校验 state/iss、换 token，返回 `Session`。
6. 调用方持久化 `Session`；之后用 `EnsureToken`/`Refresh` 管过期，用 `Revoke` 断开。

`Session` 自带 token endpoint 与 revocation endpoint，刷新不要求重新发现；`AuthorizationServer` 是 issuer，可用于回调校验。

## 客户端形态

包对服务商类型没有假设，客户端凭据完全按元数据与注册结果走：

- 服务商声明 `none`（公开客户端，MCP 规范推荐 + PKCE）时优先用 `none`，凭据只有 client_id。
- 服务商只支持机密客户端时，DCR 直接请求 `client_secret_basic`，注册响应下发的 `client_secret` 存进 `Session.Client`，后续交换与刷新都用 Basic 头（`client_id:secret` 先 urlencode 再 base64）。
- 用户手工登记 client 时填 `RegisterOptions.ClientID/ClientSecret`，方法留空则按元数据挑 basic/post；不触发 DCR。
- 服务商没有 `registration_endpoint` 时 `Register`/`Begin` 返回 `*RegistrationError`，消息里带出要加白的 loopback 回调地址。

`Refresh` 把 `invalid_grant`（refresh token 失效/已轮换）包成 `ErrReauthRequired`，同时保留原始 `*OAuthError`，调用方 `errors.Is` 判重新授权、`errors.As` 取错误码。

## 边界

- 不实现 MCP HTTP 传输、Bearer 注入、401 重试——这些在调用方的 transport 里。
- 不实现 token 持久化；`Store` 只有内存实现，文件/Keychain 由调用方提供。
- 不打开浏览器；`Flow` 只返回 URL。
- DCR 之外的手工 client、非 loopback 的公网回调属于调用方/后续扩展。

## 安全

- 远程端点必须 HTTPS；只有 loopback 允许 HTTP（本地开发和测试）。
- 回调只监听 `127.0.0.1`，一次性，state 常量时间比较；可选校验 `iss`。
- `resource` 参数按 RFC 8707 同时出现在授权与 token 请求里。
- 包不打印 token、code、client secret；错误只带状态码与 OAuth 错误码。
- `client_secret` 落在 `Session` 里，怎么存（0600 文件 / Keychain）由调用方决定；用 `Session.Client` 落盘时按机密凭据对待。

## 测试

`auth_test.go` 用 `httptest` 假授权服务器（`fake_as_test.go`）覆盖发现、DCR、手工 client、机密客户端 basic/post 选路、PKCE、state、iss、回调、token 交换、刷新与错误映射、撤销，不访问真实网络。Notion 只是其中一个真实服务形态，包内没有服务商分支。
