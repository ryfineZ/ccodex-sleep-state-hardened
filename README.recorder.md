# 独立请求记录与模型证据工具

版本：`0.5.0-state-cookie`。入口：`cmd/ccodex-request-recorder`。

这是单独运行的应用层 HTTP/SSE 记录器，不是系统抓包或 HTTPS 中间人代理。不会安装证书、修改系统代理或自动重放请求。默认只记录；开启首页核心开关后支持受控采集、缓存和注入 state + Cookie，完整规则见 [核心说明](docs/state-cookie-core.md)。模型改写默认关闭。一键模式经页面按钮确认后读取当前 Codex 配置、仅识别登录类型，并用备份事务临时接入；不导入、刷新或修改登录令牌。普通独立模式不读取 Codex 登录文件。

## 推荐：双击启动

1. 双击根目录 `start.command`（Mac）或 `start.cmd`（Windows）。自动打开并登录面板，不用复制令牌。
2. 点击「一键接入 Codex」。自动识别原上游、认证方式和默认 profile，保留模型，备份并临时修改提供商连接。不会发送模型探测。
3. 需要核心功能时，在首页勾选「开启采集与注入」并应用；再重启 Codex、新建会话正常使用。只想抓包无需开启。

点击「恢复并退出」即可停用；正常 Ctrl+C 退出也恢复原连接。重复双击会重新打开已运行的面板，不启动第二份。启动阶段本身不改 Codex 配置，首次接入由按钮明确触发。

联网方式位于折叠的「连接设置」中。自动模式保留记录器配置里已有的代理；没有配置代理时，仅向本机 7897、7890、10808 三个端口发送 SOCKS5 问候，未找到时直连。可明确选择直连或填写 HTTP/HTTPS/SOCKS 代理。自动检测不读取代理软件账号库，不扫描网络，不向模型上游发送请求。

如果上次异常退出，面板提示先恢复原连接。原配置被 CCS 等程序修改时停止转发，保留备份并拒绝覆盖。先停止其他正在接管 Codex 的工具；无法安全识别的上游路径或认证配置不会被猜测替换。应用命令行的独立 profile、项目级覆盖和其他 App 连接不自动接管。

一键模式启动参数（通常不需要）：`setup -codex-home <目录> -profile <名称> -no-browser`。默认跟随 `CODEX_HOME`，否则使用用户的 `.codex`。启动器仅打开一次性、有效一分钟的登录链接，永久管理令牌不会放进浏览器命令参数。

## 高级：手动独立启动

在项目根目录执行：

```sh
./bin/ccodex-request-recorder -config config/request-recorder.json -check
./bin/ccodex-request-recorder -config config/request-recorder.json
```

从源码手动运行可用 `./scripts/run-recorder.sh`。`scripts/start-recorder.command` 现在转入根目录的一键入口。双击入口优先使用已有二进制，缺失时才尝试 Go 编译；拉取新源码后请先重新构建。

默认端口 `127.0.0.1:17843`，默认上游为 `https://chatgpt.com`。启动输出包含带本地访问令牌的面板地址，打开完整地址即可。令牌放在 URL fragment，页面读取后从地址栏移除；不要分享这个地址。`/healthz` 不发送上游请求。

以下地址配置只针对手动独立模式；一键模式会自动处理，无需照抄。客户端的 Codex API 基础地址需要明确指向 `http://127.0.0.1:17843/backend-api/codex`，记录器才能看见请求。仅启动记录器不能看到其他 App 的原有连接。

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

记录目录默认为 `.local/recordings`，macOS/Linux 使用目录 0700、文件 0600；Windows 在创建记录目录时设置受保护的 DACL，只授予当前进程用户及 SYSTEM 访问，子文件继承该 ACL。写入敏感内容前及读取已有记录时均检查实际打开的文件句柄。拒绝不安全目录、记录符号链接及 Windows 重解析点；不会自动放宽或重设已有目录权限。默认每方向只保留前 2 MiB，最多可配置 8 MiB；解压分析另有 16 MiB 上限。SSE 单事件 256 KiB，事件展示最多 1024 条，额度快照最多 64 条。超限明确标记，不能称为完整抓包。

核心采集和模型改写均关闭时，前台仅作有界内存取样，后台串行分析和写盘。开启改写时，生成请求需要在转发前有界缓冲和解析。默认磁盘 256 MiB、2000 条记录。队列/磁盘额度满或写盘失败会增加丢弃计数，不阻塞正常回复，也不静默删除旧证据。暂停仅影响之后开始的记录，正在进行的记录保留，转发继续。

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

## Windows 存储兼容性（0.3.2）

Windows 不用 `chmod 0700/0600` 判断保密性，而使用原生 ACL。需要支持 ACL 的文件系统（例如 NTFS）；不能验证权限时停止记录器启动。已经存在的共享目录不会被程序擅自修改：为 `directory` 配置一个尚不存在的专用子目录，再启动以便程序原子创建私有目录。不要直接使用 Downloads、工作区根目录或多用户共享目录。

保护目标是阻止普通其他用户读取抓包文件，不对当前用户的其他程序、Windows SYSTEM 或拥有系统管理特权的管理员提供隔离，也不构成磁盘加密。模型改写仍默认关闭；本次没有恢复模型别名功能。

## 一键模式的安全与退出

单实例锁与恢复事务位于记录目录同级的 `recorder-control` 私有目录。运行标记包含管理令牌，不应分享或提交到仓库。Codex 备份沿用原项目的校验与语义恢复机制，可能包含用户配置，须保管在本机。

程序启动或打开面板不会自动发送模型请求；只有用户在 Codex 发送的请求会转发。接管期间，连接或认证类别变化会阻止新请求。正常退出先关闭服务、完成或中止当前请求，再恢复配置；恢复遇到冲突时提示并留下事务，不能声称自动恢复成功。

一键模式支持常见 `/backend-api/codex`、`/v1` 基础路径。自定义其他基础路径需要先调整允许列表，无法安全识别时会停在面板，而不是发送到猜测的服务器。此版本不承诺真实账号或任意 Codex App 版本的兼容性；本次端到端验收使用隔离的临时 Codex 配置与本地模拟上游。
