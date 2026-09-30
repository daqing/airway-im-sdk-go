# airway-im-sdk-go

[Airway IM](https://github.com/daqing/airway-im-plugin) 的 Go SDK：把后端
REST API、WebSocket 网关协议和服务端凭据铸造端点封装成开箱即用的 Go
接口，Go 后端与 Go 客户端应用无需自行实现凭据、网关首帧认证、心跳、
指数退避重连和基于 sequence 的补齐同步。

仅依赖 **Go 标准库**——零第三方依赖，RFC 6455 WebSocket 客户端也是
内置实现。要求 Go 1.27+。与其他 Airway IM SDK 按同一节奏发版（当前
0.7.0）；公开 API 与 `airway-im-sdk-ts`、`airway-im-sdk-swift` 一一对应。

## 特性

- **REST 全覆盖** — 资料、会话（直聊 get-or-create / 群聊 / 成员管理）、
  消息（历史、发送、幂等重试）和文件上传，全部强类型，自动解包
  `{code, data, message}` 信封。
- **会话句柄** — `CreateDirect` / `GetDirect` / `CreateGroup` /
  `OpenConversation` 返回按会话缓存的对象（`*DirectConversation` /
  `*GroupConversation`），带会话级 `OnMessage` 事件与 `Send` /
  `History`；会话类型体现在具体类型上，第一个消息监听器自动开始跟踪。
- **实时网关** — 自动完成 `{"cmd":"auth"}` 首帧认证、应用层心跳（默认
  25 秒）、指数退避重连（0.5 秒 → 10 秒）和按 `event_id` 去重。
- **同步引擎** — 维护每会话的 `sequence` 游标：收到 `message.created`
  事件时通过 `after_sequence` 补齐缺口，乱序、重复、离线期间漏掉的消息
  最终都会按序、不重不漏地从 `OnMessage` 送达。游标可通过自定义
  `SequenceStore` 持久化，重启后只拉增量。
- **凭据续期** — 凭据过期（HTTP 401/10001 或网关认证失败）时，SDK 自动
  调用 `WithGetCredential` 回调换取新凭据并恢复，业务代码无感。
- **服务端凭据铸造** — Go 后端通过 `InternalClient`（服务对服务、仅限
  私有网络；切勿链入客户端代码）获取凭据，不必手写 HTTP 调用。
- **管理操作** — `AdminClient` 覆盖管理控制台 API（状态、用户、吊销、
  消息审查、审核），会话管理全自动。
- **凭据助手** — `SignCredential` / `DecodeCredential` /
  `VerifyCredentialSignature` / `IsCredentialExpired`，供 Airway 项目
  自身一侧使用。

## 安装

包位于本仓库 `install/ignore/sdk/go/`（module
`github.com/daqing/airway-im-sdk-go`）；与其他 Airway IM SDK 按同一节奏
打 tag 发布（当前 0.7.0）。开发期在 `go.mod` 里用 `replace` 指向本地
目录，发布后 `go get` 指向发布仓库即可：

```bash
go get github.com/daqing/airway-im-sdk-go
```

## 快速上手

### 1. 由你自己的后端签发并下发凭据

SDK 不含登录逻辑。用户先在你的平台登录（密码、短信验证码……）；登录
成功后，**你平台自己的服务端**从自己的用户表读出 `(uuid, name)`，通过
内部铸造端点（由 `IM_INTERNAL_SECRET` 保护）**服务对服务**地换取凭据，
随你自己的登录响应一起返回给客户端。Go 后端用 `InternalClient` 完成
（仅服务端使用，切勿链入客户端代码）：

```go
internal := airwayim.NewInternalClient(
    "http://127.0.0.1:1906", // 内部监听地址，仅限私有网络
    os.Getenv("IM_INTERNAL_SECRET"))

minted, err := internal.MintCredential(ctx, airwayim.MintOptions{
    UUID:     user.UUID,        // 来自你自己的用户表
    Name:     user.Username,
    Nickname: user.DisplayName, // 可选；非空才写入
})
// minted.Credential — "im1.…"，随你的登录响应下发给客户端
// minted.ExpiresAt  — RFC 3339 UTC，到点重新铸造；服务凭据为空
```

其他语言的后端用普通 HTTP 调同一端点。签名密钥 `IM_AUTH_SECRET` 只存在
于 Airway 项目自己的 IM 服务端；你的后端只需要 `IM_INTERNAL_SECRET`。
客户端不持有任何密钥，只持有成品凭据（REST：`Authorization: Bearer`；
WebSocket：首条 `auth` 命令）。

**前提：你的平台必须有自己的服务端。** 纯客户端应用没有服务端无法安全
集成——客户端没有安全获取凭据的途径。完整信任模型见
[`deps/im/docs/design/identity.md`](../../../deps/im/docs/design/identity.md)。

### 2. 在 Go 应用里收发消息

```go
im := airwayim.New(
    "https://im.example.com", // IM 后端（:1905），生产环境用 https
    "wss://im.example.com",   // WebSocket 网关（:1910）；SDK 自动连接 <wsURL>/ws
    storedCredential,
    airwayim.WithGetCredential(fetchNewCredentialFromYourBackend), // 凭据失效时自动调用
)
defer im.Close()

im.OnStatus(func(status airwayim.ConnectionStatus) { log.Println("connection:", status) })
im.Connect()

// 直聊：会话类型体现在具体类型上
direct, err := im.CreateDirect(ctx, otherUserUUID) // get-or-create
direct.OnMessage(func(message airwayim.ChatMessage) {
    // 有序、去重、补齐缺口（包括离线期间漏掉的消息）；
    // 第一个监听器自动开始跟踪
    log.Println(message.Sender.Username, message.Content)
})
backlog, err := direct.History(ctx, airwayim.HistoryOptions{}) // 迄今的积压，按 sequence 升序

// 发送（SDK 自动生成 Idempotency-Key，并在网络失败时用同一个 key 重试，
// 消息不会因重试而重复）
_, err = direct.Send(ctx, "你好", &airwayim.SendOptions{ContentType: airwayim.ContentTypePlain})

// 群聊：同一套模型
group, err := im.CreateGroup(ctx, ptr("Team"), []string{otherUserUUID})
group.OnMessage(func(message airwayim.ChatMessage) { log.Println(message.Content) })
_, err = group.Send(ctx, "hello", nil)
_, err = group.AddMembers(ctx, []string{anotherUserUUID})
```

### 3. 长驻进程

`Connect` 是幂等的——进程恢复网络后再次调用即可；SDK 自动重连并补齐
同步。所有方法并发安全，整个应用共享一个 `Session` 即可。会话不再使用
时，`Close` 会断开网关并停止事件泵。

## API 参考

### `airwayim.New` 选项

| 参数 | 必填 | 默认 | 说明 |
| --- | --- | --- | --- |
| `apiURL` | ✓ | — | IM 后端 URL（:1905）；生产环境 https |
| `wsURL` | | — | 网关基础 URL（`:1910`）——**不要带路径**，SDK 自己追加 `/ws`；传 `""` 表示只用 REST |
| `credential` | ✓ | — | 你的后端签发的用户凭据 |
| `WithGetCredential` | | — | 当前凭据被拒（HTTP 401/10001 或网关认证失败）时换取新凭据；每次请求最多续期一次、重试一次 |
| `WithTimeout` | | `15s` | REST 请求超时 |
| `WithPingInterval` | | `25s` | 应用层心跳间隔，`0` 关闭 |
| `WithPersistSequences` | | `true` | 持久化每会话 sequence 游标 |
| `WithSequenceStore` | | 进程内 | 游标存储位置（`SequenceStore` 接口，可接文件/数据库/redis） |
| `WithHTTPTransport` / `WithSocketFactory` | | net/http / RFC 6455 | 自定义传输接缝，用于测试与代理 |
| `WithAutoConnect` | | `false` | 创建后立即连接网关 |

### REST 方法（`im.*`）

| 方法 | 端点 |
| --- | --- |
| `im.Me(ctx)` | `GET /api/v1/me` |
| `im.ListGroups(ctx)` | `GET /api/v1/conversations?type=group` |
| `im.CreateDirect(ctx, otherUserUUID)` | 直聊 get-or-create；返回 `*DirectConversation` 句柄 |
| `im.GetDirect(ctx, otherUserUUID)` | 已存在的直聊句柄（无则 nil） |
| `im.CreateGroup(ctx, title, memberUUIDs)` | `POST /api/v1/group`；返回 `*GroupConversation` 句柄 |
| `im.OpenConversation(ctx, id)` | 按 id 打开任意会话句柄（类型来自注册表，否则一次 REST 查询） |
| `im.AddMembers(ctx, conversationID, memberUUIDs)` | `POST .../members` |
| `im.RemoveMembers(ctx, conversationID, userUUIDs)` | `DELETE .../members/:user_uuid` |
| `im.History(ctx, conversationID, opts)` | 拉取历史 + 开始跟踪同步 |
| `im.ListMessages(ctx, conversationID, opts)` | `GET .../messages`（原始分页） |
| `im.SendGroupMessage(ctx, conversationID, content, opts)` | `POST /api/v1/messages` |
| `im.SendDirectMessage(ctx, otherUserUUID, content, opts)` | get-or-create 直聊会话后 `POST /api/v1/messages` |
| `im.UploadFile(ctx, input)` / `im.UploadFileFromPath(ctx, path, dir)` | `POST /api/v1/storage`（multipart） |
| `im.StorageURL(key)` | 文件下载 URL |

发送选项：`ContentType`（默认 `ContentTypeMarkdown` /
`ContentTypePlain`）、`IdempotencyKey`（默认自动生成）、`Retries`
（网络失败重试次数，默认 1）。

以上方法在 `im.REST`（`*RESTClient`）上同样可用，便于无会话的纯 REST
集成。

### 会话句柄（`*DirectConversation` / `*GroupConversation`）

句柄是按会话缓存的对象——类型即会话类型，同一会话总是返回同一个句柄。
句柄上出现的事件同样会出现在门面的全局流上（见下），反之亦然。

| 成员 | 说明 |
| --- | --- |
| `ID()` / `Kind()` | 会话 id；`direct` 或 `group` |
| `OnMessage(…)` / `OnMessageUpdated(…)` | 有序消息事件；群聊另有 `OnMembersAdded` / `OnMembersRemoved`。第一个 `OnMessage` 监听器自动开始跟踪（从上次持久化游标拉历史，之后实时）。每次注册返回带 `Cancel()` 的 `*Subscription` |
| `History(ctx, opts)` | 等待积压拉完；每条消息经 `OnMessage` 派发 |
| `Send(ctx, content, opts)` | 向本会话发送（幂等/重试语义同 `SendGroupMessage`） |
| `ListMessages(ctx, opts)` | 原始有序分页，不改状态 |
| `LastSequence()` / `Forget()` | 同步游标；丢弃本会话全部状态 |
| `Details(ctx)` | 会话类型 + 带角色的成员列表，实时来自 API |
| 仅群聊：`Title` | 创建时的标题（按 id 打开时为 nil） |
| 仅群聊：`AddMembers(…)` / `RemoveMembers(…)` | 成员管理；返回带角色的成员列表 |

### 全局流（`im.On…`）

会话句柄是按窗口的 API；门面同时暴露全局流，适合未读角标、统一收件箱
等场景：

| 注册方法 | 载荷 | 说明 |
| --- | --- | --- |
| `OnMessage` | `ChatMessage` | 新消息，有序、去重、补齐缺口 |
| `OnMessageUpdated` | `ChatMessage` | 消息被审核遮蔽；content 已是 `***`，替换渲染即可 |
| `OnMembersAdded` | `MembersAddedInfo` | 群成员新增（被加入的用户自己也会收到） |
| `OnMembersRemoved` | `MembersRemovedInfo` | 群成员被移除（被踢的用户也会收到） |
| `OnStatus` | `ConnectionStatus` | `connecting / authenticating / online / reconnecting / offline / closed` |
| `OnError` | `*IMError` | 网关认证失败、凭据续期失败等 |
| `OnEvent` | 原始 `GatewayEvent` | 每一帧网关事件；尚未跟踪的会话的消息也只在这里出现 |

连接控制：`im.Connect()`（幂等）/ `im.Disconnect()` / `im.IsOnline()` /
`im.ConnectionStatus()` / `im.SetCredential(_)` /
`im.LastSequence(conversationID)` / `im.ForgetConversation(conversationID)`
（例如被踢出群后丢弃同步游标）。

### 消息模型（`ChatMessage`）

`Send`、`SendDirectMessage`、`ListMessages`、`History` 和实时
`OnMessage` 事件携带的都是同一个 `ChatMessage`：

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `ID` | string，26 位 ULID | 服务端生成的全局唯一消息标识；稳定的去重键。 |
| `ConversationID` | string，26 位 ULID | 消息所属会话；按它把消息路由到聊天窗口。 |
| `Sender.UUID` | string | 作者的稳定身份 uuid（API 与事件全程只以 uuid 标识身份）。 |
| `Sender.Username` | string | 宿主分配的账号名，账号生命周期内稳定。 |
| `Sender.Nickname` | string? | 首选显示名；为空时回退到 `Username`。 |
| `Sender.AvatarURL` | string? | 头像 URL；为空时渲染占位图。 |
| `Content` | string | 消息体，1–32768 UTF-8 字节，服务端已把 CRLF 归一为 LF。被审核的消息读回为字面量 `***`。 |
| `ContentType` | string | `text/markdown`（默认）或 `text/plain`——`Content` 的渲染方式。 |
| `CreatedAt` | string，RFC 3339 UTC | 服务端提交时间戳；展示时换算到查看者时区。 |
| `Sequence` | 整数 ≥ 1 | 会话内位置，提交时分配。`(ConversationID, Sequence)` 即全序。 |

按 `Sequence` 排序，永远不要按 `CreatedAt`。同一幂等键的重试发送返回
原消息（相同 `ID` 与 `Sequence`）；SDK 已为你完成实时事件的去重与补齐。
`Sender` 反映作者当前资料，不是发送时刻的快照。

### `InternalClient`（内部铸造端点，默认 `127.0.0.1:1906`）

| 方法 | 说明 |
| --- | --- |
| `MintCredential(ctx, MintOptions)` | 服务对服务凭据铸造；`TTLSeconds` 为 nil 时用后端默认值（86400，上限 2592000），`0` 表示不带过期；返回 `MintedCredential`（`Credential`、`ExpiresAt`） |

此类型仅供你的后端使用——客户端应用绝不能调用：能铸造凭据者可以冒充
任何用户。这里的错误码 `10005` 表示 `X-IM-Internal-Secret` 请求头缺失
或错误（不是公开 API 的权限拒绝）；`10006` 表示部署未配置
`IM_AUTH_SECRET`，无法签名。

### `AdminClient`（管理控制台，`/admin/api`）

首次调用自动登录（12 小时会话）；会话中途 401 会自动重登一次并重试。
响应为 `JSONValue`（管理端结构开放；见
[`deps/im/docs/api/admin.md`](../../../deps/im/docs/api/admin.md)）。

| 方法 | 说明 |
| --- | --- |
| `Login` / `Logout` | 显式登录 / 登出 |
| `Status` | 聚合状态：用户、发件箱、网关/投递指标 |
| `Users` | 已注册用户（含 `token_version`） |
| `RevokeUser(uuid)` | 吊销某用户全部凭据并踢下线 |
| `GroupConversations` | 所有群会话（含成员/消息数） |
| `ConversationMessages(id)` | 消息审查（升序） |
| `MarkIllegal(messageID)` | 标记违规（幂等）：客户端侧内容遮蔽为 `***` |

### `SignCredential`（签名与校验助手）

本地签名仅供 Airway 项目自身一侧、可信持有 `IM_AUTH_SECRET` 者使用；
第三方平台后端不持有该密钥，必须改用 `InternalClient`。

| 函数 | 说明 |
| --- | --- |
| `SignCredential(secret, uuid, name, opts)` | 签发 `im1.<payload>.<sig>` HMAC-SHA256 凭据；可选字段仅在非空时写入，不会覆盖已有资料。`TTLSeconds`：0 用默认 24 小时，负数省略 `exp`（仅限服务凭据） |
| `DecodeCredential(credential)` | 返回 `CredentialClaims`（畸形输入报错） |
| `VerifyCredentialSignature(credential, secret)` | 常数时间校验，畸形输入返回 `false` |
| `IsCredentialExpired(credential, now)` | `exp` 是否已过；无 `exp` 的凭据永不过期 |

### 错误处理

所有 REST 错误都是 `*IMError`（`Code` —— 信封业务码，`Status` —— HTTP
状态码，传输失败时 `Status == 0`）。常见判断：

```go
message, err := im.SendGroupMessage(ctx, id, "hi", nil)
var imError *airwayim.IMError
if errors.As(err, &imError) {
    if imError.IsAuthError() { /* 10001：凭据无效/过期——等自动续期或重新登录 */ }
    else if imError.Code == airwayim.CodePermissionDenied { /* 10005：不是成员 */ }
    else if imError.Code == airwayim.CodeConversationNotFound { /* 11001 */ }
}
```

完整错误码：`10000` 内部错误、`10001` 凭据无效、`10003` 请求非法、
`10005` 权限拒绝、`11001` 会话不存在、`11002` 幂等键被不同请求复用。

## 消息可靠性模型

后端保证每会话 `sequence` 单调递增；投递是**至少一次**。SDK 的同步
引擎负责按 `event_id` / `message_id` 去重、按 `sequence` 排序、经
`after_sequence` 补齐缺口。业务代码只需要：

1. 打开会话——句柄的第一个 `OnMessage`（或 `History`）即开始跟踪；
2. 在 `OnMessage` 事件里追加渲染（用 `message.ID` 幂等地处理重复消息）；
3. 用 `Send` 的返回值做乐观渲染，并按 `ID` 去重实时回声。

从未跟踪的会话不会自动拉取消息（避免冲刷全部历史）；未读角标等场景用
`OnEvent` 处理。

## 并发

`Session`、`RESTClient`、`InternalClient`、`AdminClient` 的所有方法都
并发安全；整个应用共享一个实例即可。事件处理器在独立的 goroutine 上
执行——触碰 UI 前请自行切回 UI 线程。`Subscription.Cancel()` 取消订阅；
监听器存续期间请持有返回的订阅对象。

## 本地开发与验证

```bash
go test -race ./...
# 58 个测试：签名向量（与 Node 参考实现逐字节一致）、信封解析、幂等
# 重试、凭据续期、同步引擎、网关重连/认证流程，以及一个手写 RFC 6455
# 服务端——仅标准库，不依赖外部服务。
```

对着真实技术栈做端到端验证（后端 :1905 / 网关 :1910 / 内部 :1906，见
仓库根 README）：启动技术栈后按"快速上手"逐步执行即可。

## 协议参考

- API 契约：[`deps/im/docs/api/openapi.md`](../../../deps/im/docs/api/openapi.md)
- 网关协议：[`deps/im/docs/design/gateway.md`](../../../deps/im/docs/design/gateway.md)
- 凭据签发：[`deps/im/docs/design/identity.md`](../../../deps/im/docs/design/identity.md)
- 管理控制台：[`deps/im/docs/api/admin.md`](../../../deps/im/docs/api/admin.md)

---

English version: [README.md](README.md)
