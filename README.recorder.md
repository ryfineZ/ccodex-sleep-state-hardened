# 独立请求记录与模型证据工具

版本：`0.3.1-model-override`。入口：`cmd/ccodex-request-recorder`。

这是单独运行的应用层 HTTP/SSE 记录器，不是系统抓包或 HTTPS 中间人代理。它不会安装证书、修改系统代理、读取 Codex 登录文件、采集或注入 state、管理 Cookie 池，或自动重放请求。只接收主动接入本地端口的客户端流量；模型改写是可选开关，默认关闭。

## 启动

在项目根目录执行：

```sh
./bin/ccodex-request-recorder -config config/request-recorder.json -check
./bin/ccodex-request-recorder -config config/request-recorder.json
```

从源码启动可用 `./scripts/run-recorder.sh`；macOS 也可双击 `scripts/start-recorder.command`。这些脚本会编译新入口，不会自动接管 Codex。Ctrl+C 停止。

默认端口 `127.0.0.1:17843`，默认上游为 `https://chatgpt.com`。启动输出包含带本地访问令牌的面板地址，打开完整地址即可。令牌放在 URL fragment，页面读取后从地址栏移除；不要分享这个地址。`/healthz` 不发送上游请求。

客户端的 Codex API 基础地址需要明确指向 `http://127.0.0.1:17843/backend-api/codex`，记录器才能看见请求。仅启动记录器不能看到其他 App 的原有连接。

## 和已有网关配合

`config/request-recorder.gateway.example.json` 将固定上游设为 `http://127.0.0.1:17842`，形成“客户端 → 记录器 → reliable-proxy → 上游”。先自行启动已有网关，再使用此示例启动记录器。

此接法观察的是已有网关的入口和返回，不是该网关内部最终出口。`route_label` 是配置标签，不是自动探测的真实公网出口。需要观察某个其他中转的实际响应时，把记录器的 `upstream` 明确设置为该中转的 origin。

`upstream` 只接受源站地址，不接受路径、查询或内嵌凭据。`proxy_url` 可显式指定已有 HTTP/HTTPS/SOCKS 代理；不扫描本机代理端口。默认不会读取环境代理。公开 API 使用时，应自行将 origin 改为相应 API 服务器，并让客户端使用 `/v1/` 路径和自己的授权方式。

## 模型检测与额度

每条记录包含请求模型、实际出站模型、上游宣告值、字段路径、事件序号、应用层时间，以及冲突或观测受限标记。支持 JSON、Responses SSE、Chat Completions 的顶层模型字段，以及已知 ChatGPT message metadata 字段；正文中的自报身份和工具输出不会当成模型证据。非标准模型响应头仅作为补充线索。

结果分为 `consistent`、`mismatch`、`conflict`、`unknown`、`observed`（请求模型未知）。`actual_execution_verified` 始终为 false：工具没有服务器内部执行证明。上游回显的模型名仍可能不是内部执行身份。


额度观测读取 primary/secondary 的 used-percent、window-minutes、reset-at、reset-after-seconds，并支持命名的 codex 限额族、`response.metadata` 和 `codex.rate_limits` 事件。缺失字段保持未知；异常数字与重复字段不强行解释。不会把 primary 固定称为周额度，也不会依据额度更换账号或自动重试。

面板支持模型结果过滤、详情、额度窗口、state/Cookie 指纹、按凭据作用域 + 配置路线 + 上游 + 请求模型的统计。统计是已保存记录的描述，不是对节点质量的因果评分。凭据作用域指纹不是经过验证的账号 ID，凭据轮换会影响分组。

## 强制模型改写（默认关闭）

模型别名配置及归一化逻辑已移除。不同代际、mini/nano、快照名等均按原始字符串比较，不自动合并。旧自定义配置若有 `model_aliases`，请删除该字段，否则严格配置校验会报未知字段。历史 JSON 不会被批量改写。

默认配置：

```json
{
  "force_model_enabled": false,
  "force_model": ""
}
```

在本地面板填写目标模型、勾选开关并点击“应用到后续请求”即可在当前进程开启。也可在配置文件中设 `force_model_enabled: true` 和非空 `force_model` 后重启。面板操作不会写回配置；重启后按配置文件恢复。每条请求在接入时取得不可变策略快照，切换不影响进行中的请求。

只改写 POST 的 `/backend-api/codex/responses`、`/backend-api/codex/responses/compact`、`/backend-api/conversation`、`/v1/responses`、`/v1/responses/compact`、`/v1/chat/completions`、`/v1/completions`。其他路径（包括模型列表、搜索、文件、嵌入）保持原样。只替换唯一顶层 `model` 的字符串值，其他 JSON 字节（嵌套字段、提示词、大整数、参数、空白和顺序）保持不变；不会往缺失 model 的请求中猜测添加。

开启后，缺少/重复 model、非字符串值、无效 JSON、不支持的请求内容类型或解压失败会在本地报错，不会悄悄绕过开关。gzip/deflate/br/zstd 请求发生模型变更时，出站改发解码后的 JSON，并同步更新长度和编码头；模型本来相同则保留原始压缩正文。改写用的解码请求上限 16 MiB，且不超过 `max_request_mib`。带正文摘要/签名或 trailers 的请求不改写，避免破坏完整性校验。

每条记录分开保留“客户端原始模型 → 出站模型 → 上游宣告模型”。`model_override` 记录开关快照、目标、是否适用和是否实际更改模型值；`incoming_request_body` 是改写前捕获，`request_body` 是出站捕获，两者均遵循当前记录模式和大小限制。模型比较以出站值为基准，主动改写单独标记，不冒充上游换模型。离线分析同样保留这三个值。

暂停记录不关闭改写，两者是独立开关。强制模型名称只是改变请求值，不保证上游内部执行身份；不会为此替换 state、Cookie、账号、出口或自动重发。与 reliable-proxy 串联时，它自身的 state/出口规则仍保持不变。

`POST /__recorder/api/model-override` 需要当前进程的 `X-Recorder-Token`。开启正文 `{"enabled":true,"model":"指定模型"}`，关闭正文 `{"enabled":false,"model":""}`；返回 `persisted:false`。启动时的 `-check` 会显示开关与目标配置。

## 记录模式与容量

| 模式 | 持久化内容 |
| --- | --- |
| `metadata` | 头部脱敏值、模型证据、额度、事件名及时间；不保存请求/回复正文。 |
| `redacted`（默认） | 结构化 JSON/SSE，隐藏已知凭据字段；仍可能保存提示词、回复及其他业务隐私。 |
| `full` | 完整头部与捕获到的原始 body 分片（Base64），仍受大小限制；必须额外传入 `-allow-sensitive-recording`。 |

记录目录默认为 `.local/recordings`，目录 0700、文件 0600；拒绝不安全目录或记录符号链接。默认每方向只保留前 2 MiB，最多可配置 8 MiB；解压分析另有 16 MiB 上限。SSE 单事件 256 KiB，事件展示最多 1024 条，额度快照最多 64 条。超限明确标记，不能称为完整抓包。

关闭模型改写时，前台仅作有界内存取样，后台串行分析和写盘。开启改写时，生成请求需要在转发前有界缓冲和解析。默认磁盘 256 MiB、2000 条记录。队列/磁盘额度满或写盘失败会增加丢弃计数，不阻塞正常回复，也不静默删除旧证据。暂停仅影响之后开始的记录，正在进行的记录保留，转发继续。

HTTP 头部规范化、跳跃头处理和 TLS 连接由 Go HTTP 转发层负责；它不是逐 TCP 包或 TLS 字节还原。原始 body 在捕获限额内保留；关闭模型改写时 gzip/deflate/br/zstd 只对分析副本解码；开启后的请求编码处理见下节。所有模式均不改写上游响应正文。分片时间是应用读取时间，超过时间采样上限或压缩事件会明确标记时间精度受限。

## 导出、离线分析和测试

请求结束后异步保存，页面每三秒刷新；当前不是实时逐 token 查看器。详情页可导出单条 JSON。导出前检查业务隐私，尤其 full 模式。切换模式不会清理已有敏感记录。

```sh
./bin/ccodex-request-recorder -analyze .local/recordings/某条记录ID.json
./scripts/test-recorder.sh
python3 scripts/smoke-recorder.py
```

离线分析不会联网或重发请求，只比较原始模型标识，忽略历史记录中的别名规则。full 模式可重新解析原始捕获片段；redacted 模式重新解析保留的结构；metadata 模式没有正文可重新解析，因此离线结论可能降为“未知/受限”，不能恢复被省略的信息。

`smoke-recorder.py` 使用编译产物和随机端口的本机模拟上游，不使用真实凭据；验证结束后关闭两个进程/服务。结果与仅含合成数据的样例记录放在 `.local/validation/`。详细测试结果见 `docs/recorder-validation.md`。

HTTP `GET /__recorder/api/status`、`GET /__recorder/api/records?limit=200`、`GET /__recorder/api/records/<id>`、`GET /__recorder/api/observations` 和 `POST /__recorder/api/capture` 都需要当前进程输出的 `X-Recorder-Token`。管理接口不能向远程开放。

## 参考与范围

`docs/ccodex-rotate-review.md` 记录了针对 ccodex-rotate 固定提交的审查、已吸收的设计和明确未移植的行为。原采集器及旧面板没有因本次独立工具而被替换；可靠转发程序仍是独立的 `ccodex-reliable-proxy`。

协议依据：OpenAI Responses streaming events，`https://developers.openai.com/api/reference/resources/responses/streaming-events`；Codex 额度解析，`https://github.com/openai/codex/blob/main/codex-rs/codex-api/src/rate_limits.rs`。这些是可观测字段的依据，不保证任一自定义上游都按同样协议返回。
