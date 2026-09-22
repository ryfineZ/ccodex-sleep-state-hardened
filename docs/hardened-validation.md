# 本机验证记录

验证时间：`2026-09-22T05:35:50+08:00`。以下记录来自实际执行结果，而非计划执行。

项目：`/path/to/ccodex-sleep-state-hardened`
基础提交：`b18fabf9ad8e9d7af7d9d0306b623ba6091a39d6`
开发分支：`hardening/route-cookie-state`
工具链：`go version go1.26.1 darwin/arm64`

## 结果

| 检查 | 实际结果 |
| --- | --- |
| 原项目基线 `go test ./...` | 原有 11 个包通过。 |
| 修改后 `go test -json -count=1 ./...` | 15 个包通过，234 个顶层测试通过，另有 155 个子测试通过，0 个失败。 |
| 新增模块 | 23 个新增顶层测试函数通过，包含表驱动和并发场景。 |
| 新增 4 个包 `go test -race -count=1` | 全部通过，未报告数据竞争。 |
| `go vet ./...` | 退出码 0，无诊断输出。 |
| 构建 `cmd/ccodex-reliable-proxy` | 成功生成 `bin/ccodex-reliable-proxy`。 |
| 配置 `-check` | 通过；该命令不启动网络服务。 |
| `-version` | `0.1.0-client-managed`。 |
| 编译产物本地启动与 `/healthz` | 通过，返回 `client-managed-state`。 |
| 健康检查时上游请求数 | 0；目标也是本地模拟服务，不是官方真实接口。 |
| SIGTERM 正常退出 | 退出码 0，没有留下运行中的测试网关。 |
| 原仓库工作区 | 仍然干净，没有修改原仓库文件。 |

顶层测试数与子测试数分列，避免把表驱动父测试与其子测试混算成独立功能数量。

## 回归场景

覆盖原始 state 与请求正文保留、禁止合成采集请求、正式生成不重放、Cookie 捕获/更新/删除/隔离、
迟到响应防覆盖、熔断后跳过不稳定出口、同 turn 出口不静默迁移、半开互斥及恢复、
HTTP 401/403/429 和 HTTP 200 SSE 限流的共享暂停、metadata 状态观察、流中断、客户端取消、
上游流读取空闲超时、超大事件观测受限、并发资源限制、重定向拦截及本地访问保护。

## 日志位置

- `.local/baseline-test.log`
- `.local/all-tests.jsonl`
- `.local/test-counts.json`
- `.local/race-test.log`
- `.local/vet.log`
- `.local/smoke-result.json`
- `.local/smoke.log`

这些验证文件保存在本机并被 Git 忽略。测试用例使用合成凭据和本地模拟上游。

## 尚未验证与交付边界

没有使用真实账号验证官方上游效果；没有读取登录文件，也没有修改现有 Codex 配置。
没有宣称延长服务器端 state 或 Cookie 的有效期，也没有通过 opaque state 判断实际模型身份。
新入口是 `cmd/ccodex-reliable-proxy`。原 `cmd/ccodex-sleep-state` 采集器及管理面板保留未接入这些模块，
自适应采集调度、旧主备池重构、实验性跨 turn 复用均不属于本次已实现范围。
默认配置为直连；代理示例需要替换为用户实际使用的代理地址，尚未测试用户真实代理。
