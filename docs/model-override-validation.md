# 模型开关修改与验收

实际验证时间：2026-09-22T12:30:17+08:00。版本：`0.3.1-model-override`。

## 实现

移除模型别名配置、映射和“别名等价”结果。模型名称按原始字符串比较。

新增 `force_model_enabled` / `force_model`。两个示例配置均默认关闭，目标为空。支持本地令牌保护的运行时开关；当前请求使用接入时快照，面板更改不写回配置文件。原始模型、出站模型、上游宣告值分别记录，主动改写不被误判为上游换模型。默认路径与 Cookie/state 处理不变。

## 实测结果

| 检查 | 结果 |
|---|---|
| `go test -json -count=1 ./...` | 18 个包通过；274 个顶层测试、178 个子测试通过，0 失败 |
| 新增开关测试 | 11 个顶层测试，覆盖默认关闭、精确顶层替换、重复字段、压缩、三段模型证据、鉴权、取消开关、请求快照与离线分析 |
| `go test -race -count=1`（modelaudit / requestrecorder / recorder command） | 全部通过，未报告数据竞争 |
| `go vet ./...` | 通过 |
| 面板脚本 `node --check` | 通过；本次未进行真实浏览器交互自动化 |
| 编译与两个配置 `-check` | 通过；均显示 `enabled=false` |
| 原记录器本地 smoke | 通过，2 次合成上游调用，无真实账号 |
| 关闭 → 开启 → 关闭 的可执行文件 smoke | 通过，3 次合成上游调用，三段模型记录与离线分析一致 |
| 上游请求自动重放 | 回归测试断言未重放 |
| 实际外部模型调用 | 本次 smoke 为 0；仅随机端口的回环模拟服务 |
| 测试服务清理 | 正常退出，无测试服务残留；未接管或重启用户原服务 |

## 文件

源码：`internal/requestrecorder/model_override.go`。回归测试：`internal/requestrecorder/model_override_test.go`。

新可执行文件：`bin/ccodex-request-recorder`。旧可执行文件和本次修改前源码备份位于 `.local/model-switch-backup-20260922-121837/`。

实际测试输出：`.local/validation/model-override-tests.jsonl`；两份 smoke 结果在同目录。`scripts/smoke-model-override.py` 可重复执行，只使用合成数据和回环地址。

启动方式与完整约束见 `README.recorder.md`。旧自定义配置需要自行移除 `model_aliases`；本次未读取或修改用户登录文件、系统代理、证书、私有自定义配置或真实抓包记录。
