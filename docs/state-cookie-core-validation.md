# 核心 State + Cookie 验证记录

版本：`0.5.0-state-cookie`。日期：2026-09-22T15:22:28+08:00。

## 本机实际结果

- 全项目：18 个包、300 个顶层测试、另有 189 个子测试通过，失败 0。
- 新增核心顶层测试：15 个；覆盖采集与注入、metadata、失败拒绝、Cookie 删除、时间上限、共享预算、并发单飞、隔离、出口恢复与迟到结果。
- 全项目 `go test -race -count=1 ./...` 通过。
- `go vet ./...` 通过；页面 JavaScript 语法检查通过。
- 编译产物通过真实浏览器操作验证，而不是仅检查页面存在。

## 隔离浏览器端到端结果

临时 Codex 配置、合成 Bearer 字符串、字面量回环 HTTP 上游、隔离 Chrome profile。没有读取用户真实登录文件、修改系统代理或调用真实模型上游。

```json
{
  "core_enabled_from_panel": true,
  "collected_state_and_cookies": true,
  "injected_formal_request": true,
  "automatic_login": true,
  "one_click_connect": true,
  "request_visible": true,
  "detail_opens": true,
  "restore_and_exit": true,
  "page_exceptions": 0,
  "version": "0.5.0-state-cookie",
  "startup_did_not_modify_codex": true,
  "second_launch_reopens": true,
  "original_config_restored_byte_exact": true,
  "synthetic_upstream_calls": 2,
  "synthetic_probes": 1,
  "synthetic_generations": 1,
  "real_upstream_calls": 0,
  "server_left_running": false
}
```

流程包含双击等价启动、一次性页面登录、接入配置、页面开启核心、首次采集 state 和两个路由 Cookie、带注入头部的正式请求、查看记录、恢复并退出。原临时配置字节完全恢复，无遗留测试服务。

## 边界

本地新鲜度是程序规则，不是上游 TTL。合成模型标识不构成真实模型身份或质量验证。真实账号、实际代理线路及跨 turn 的服务端接受情况未验收。Windows/Linux 的本次结果以发布后对应提交的 GitHub CI 为准。

复现：`go test -race ./...`；编译记录器后执行 `python3 scripts/smoke-core.py`（浏览器验收需要本机 Chrome 和 Node）。
