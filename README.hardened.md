# ccodex-sleep-state-hardened

本机独立开发副本，基于上游提交 `b18fabf`。新增可运行入口是 **`cmd/ccodex-reliable-proxy`**。
原入口 `cmd/ccodex-sleep-state`、采集器和管理面板仍保留作为对照，没有声称它们已经完成全部重构。

## 当前交付范围

这是一个 **客户端管理 turn-state 的 HTTP/SSE 兼容网关**，不是延长服务端 state 寿命的工具。
它不自行采集、交换或替换 state，不解密 state，不通过形状推断实际模型，也不自动重放正式生成。
客户端提供什么 state 就转发什么；新 turn 没有 state 时，不注入上一轮的 state。

| 已实现 | 行为 |
| --- | --- |
| 独立代理健康状态 | 连续传输失败触发熔断，冷却后只放行一个半开请求；旧请求结果不能覆盖更新的健康状态。 |
| 不稳定代理隔离 | 新请求跳过熔断节点；恢复尝试使用真实请求，不发送额外模型探测。 |
| turn 出口一致性 | 已知 state 固定在首次记录的出口。出口暂不可用则本地返回 503，不在客户端不知情时移动该 turn。 |
| Cookie 生命周期 | 只维护 `__cflb` / `__oailb`，按凭据、工作区、源站、出口隔离，处理更新、删除和过期；旧响应不能覆盖更新版本。 |
| SSE 观测 | 有界增量解析，识别完成、流内失败、限流及 metadata 中的 state；正文原样转发。 |
| 断流与超时记账 | 响应已经开始后断流也能记录；客户端取消不处罚代理；上游长时间无数据会关闭该流。 |
| 共享上游停止规则 | HTTP 401/403/429，以及已识别的 SSE 限流，会停止同一凭据后续请求；不会靠换出口重试。 |
| 资源及本地安全限制 | 仅监听回环地址，限制请求大小、并发、响应头时间与流读取空闲时间；日志不写凭据、Cookie、state 或正文。 |

**尚未实现：**旧采集器的主备池改造、旧管理页面集成、实验性跨 turn 复用、自适应采集调度，以及使用真实账号的上游验收。
这些不应被理解为本次已经完成的功能。

## 运行

本项目保留上游 Go 模块，需要 Go 1.26 或兼容版本。本机验证工具链为 Go 1.26.1 / macOS arm64。

```sh
cd /path/to/ccodex-sleep-state-hardened
mkdir -p bin
go build -o bin/ccodex-reliable-proxy ./cmd/ccodex-reliable-proxy
./bin/ccodex-reliable-proxy -config config/reliable-proxy.json -check
./bin/ccodex-reliable-proxy -config config/reliable-proxy.json
```

也可执行 `./scripts/run-hardened.sh`。默认使用 **直连**，监听 `127.0.0.1:17842`，不占用原项目常用的 17841 端口。
前台运行后，用另一个终端检查：

```sh
curl --fail http://127.0.0.1:17842/healthz
```

`/healthz` 只显示本地统计，不发上游请求。按 `Ctrl+C` 停止。
程序不读取 `~/.codex/auth.json`，不导入账号，不写入 Codex 配置，不安装 launchd 服务。
需要接入客户端时，使用独立的 HTTP/SSE provider，把其基础地址指向
`http://127.0.0.1:17842/backend-api/codex`，并保留客户端正确的官方认证设置；不要直接覆盖已有配置文件。
当前不支持 WebSocket，也不负责自动转换旧采集器的配置。

## 配置代理

示例中的代理端口只是占位值，运行前应改成你确实有权使用、且正在监听的代理地址。

```sh
cp config/reliable-proxy.proxies.example.json config/local.json
# 编辑 config/local.json 中的 proxy_url
./scripts/run-hardened.sh config/local.json
```

只有 `routes` 中明确列出的出口才会被使用。配置中没有 `direct` 路由时，不会静默绕过代理改成直连。
不从环境变量自动导入代理。不扫描、导入或测试用户的其他代理配置。
可使用 HTTP、HTTPS、SOCKS5/SOCKS5H 代理 URL。带认证信息的本地配置不要提交到仓库；`config/local*.json` 已忽略。

| 字段 | 默认 | 含义 |
| --- | --- | --- |
| `failure_threshold` | 2 | 连续、可归因的传输失败达到此值后熔断。 |
| `circuit_base_seconds` | 30 | 初次熔断等待时间，含少量随机抖动。 |
| `circuit_max_seconds` | 300 | 失败后的指数退避上限。 |
| `session_cookie_seconds` | 180 | 没有服务端过期字段的会话 Cookie 本地保留上限；不是推定的服务端寿命。 |
| `max_request_mib` | 64 | 单请求正文上限。 |
| `response_header_seconds` | 30 | 等待上游响应头的上限。 |
| `stream_idle_seconds` | 90 | 正在读取上游响应体时，连续无数据的上限；不是整次生成时限。 |
| `max_concurrent_requests` | 32 | 最大并发转发数量，满时直接返回本地 503。 |

180 秒、30 秒等参数是工程起点，不是由两张截图证明的最优参数。

## 如何理解 state 的时效

这里不把可解析的封装或本地一小时计时当作上游可用性保证，也不延长服务器签发的有效期。
本地只保留 state 的哈希与出口对应关系，用于维持请求路由；这些记录的清理期限不是 state 的寿命。
进程重启或记录被清理后，无法推断旧 state 曾使用的原始出口；较稳妥的迁移方式是从新的客户端 turn 开始。

两张截图证明的是特定组合在一定时间内可复用，以及同次实验中整组 Cookie 与票不必一一绑定。
它们不足以确定单独 Cookie/state 的 TTL，也不能证明跨账号、跨出口或跨 turn 复用受到协议保证。
因此当前入口不实现这种扩展。

## 验证

```sh
./scripts/test-hardened.sh
```

新增回归测试使用本地模拟服务和合成凭据，包含 Cookie 隔离/删除、迟到响应、半开互斥、正式生成不重放、
跨 turn 账号暂停、HTTP 200 SSE 限流、metadata、流中断、取消、空闲超时、超大事件、并发与本地访问限制。
具体本机验证结果见 `docs/hardened-validation.md`。

SSE 单事件观测上限为 256 KiB。超过此大小的事件仍按原字节转发，但统计标为 `observation_limited`，不冒充“验证成功”。
完整 HTTP 5xx 或明确的应用失败，不会被直接当作代理网络故障。干净 EOF 却缺少 SSE 完成事件，记作不完整流，
也不单凭这一点认定是代理的责任。

## 代码

```text
cmd/ccodex-reliable-proxy/    新的独立命令入口
internal/reliableproxy/      兼容转发、请求准入、作用域、SSE 观测
internal/routehealth/        独立熔断器与请求版本隔离
internal/cookiebundle/       内存 Cookie 管理与迟到响应保护
config/reliable-proxy.json   无凭据的默认配置
scripts/                    构建、运行、测试入口
```

架构和边界见 `docs/hardened-architecture.md`。原项目许可证与第三方声明保留，见 `LICENSE` 和 `THIRD_PARTY_NOTICES.md`。
