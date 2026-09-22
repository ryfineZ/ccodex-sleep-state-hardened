# 独立记录器本机验证记录

验证日期：2026-09-22。以下是实际执行结果，不是待执行计划。

项目：`/path/to/ccodex-sleep-state-hardened`。
分支：`hardening/route-cookie-state`。工具链：`go version go1.26.1 darwin/arm64`。
编译入口：`cmd/ccodex-request-recorder`。产物：`bin/ccodex-request-recorder`。
版本输出：`0.3.0-observability`。

## 构建和回归测试

| 检查 | 实际结果 |
| --- | --- |
| `go test -json -count=1 ./...` | 18 个包通过，266 个顶层测试通过，另有 168 个子测试通过，0 个失败。 |
| 记录器相关测试数量 | modelaudit、requestrecorder、独立 CLI 共 32 个顶层测试通过。 |
| 三个记录器相关包 `go test -race -count=1` | 全部通过，未报告数据竞争；未据此宣称全仓库都做了竞态检测。 |
| `go vet ./...` | 退出码 0，无诊断输出。 |
| `node --check internal/requestrecorder/web/app.js` | 通过。 |
| `go build -trimpath` | 成功生成本机可执行文件，约 9.9 MiB。 |
| 默认与串联网关示例配置 `-check` | 两份配置通过；检查模式不启动服务。 |
| `git diff --check` 与三个启动/测试脚本 `sh -n` | 通过。 |
| 原 `ccodex-sleep-state` 仓库工作区 | 仍然干净，没有改动原仓库文件。 |

测试数量将顶层测试和子测试分开统计，不将父测试与子测试混称为独立功能数。

## 编译产物端到端验证

实际执行 `python3 scripts/smoke-recorder.py`：启动一个随机端口的本机模拟上游和编译后的记录器，不使用真实账号或官方服务。

健康检查及面板资源访问通过，启动和健康检查产生 0 个上游请求。无管理令牌读取记录被拒绝。第一条合成请求被原样转发并记录，正确识别请求模型与返回模型不同，保留 10080 分钟额度窗口；分组统计和离线模型分析通过。暂停记录后第二条请求仍正常转发，但没有新增记录。

合成上游共收到 2 次请求，恰好对应 2 次客户端请求，没有自动重放。真实上游请求数为 0。SIGTERM 正常退出，退出码 0，没有留下运行中的测试记录器或模拟上游。

## 结果文件

- `.local/validation/all-tests-recorder.jsonl`：全项目 Go JSON 测试日志。
- `.local/validation/recorder-test-summary.json`：从实际日志计算的测试统计。
- `.local/validation/recorder-race-final.txt`：记录器相关包竞态测试结果。
- `.local/validation/vet-recorder.txt`：静态分析输出，成功时为空。
- `.local/validation/recorder-smoke.json`：编译产物端到端测试摘要。
- `.local/validation/recorder-smoke-record.json`：仅含合成内容的示例记录。

## 验证边界

本轮没有读取登录文件，没有修改系统代理、证书信任、Codex 配置，没有启用真实敏感信息完整记录，也没有替换或重启原可靠转发服务。

页面通过 JavaScript 语法检查、资源结构测试和实际 HTTP 访问验证，但未进行真实浏览器的逐控件操作或视觉验收。HTTP/SSE 结果来自本地模拟上游，不代表真实账号端到端兼容性已验证。

模型检测证明的是捕获到的上游声明，不是内部实际模型执行身份。额度字段是观测快照，不是实时余额。记录大小、事件数、时间采样及磁盘配额限制仍然有效；截断与丢弃必须按页面和记录中的标记解释。
