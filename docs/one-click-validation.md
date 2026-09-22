# 0.4.0 一键记录器：实现与验收

## 对照原项目

原项目 `scripts/start.command` / `start.cmd` 调用 `setup`，通过浏览器一次性启动凭证自动进入面板；`internal/codexconfig` 保存原配置、安装快照和事务，退出后恢复；`internal/service/quick_setup.go` 把连接设置与本机代理发现收进页面。

本次在新记录器采用同样的操作层次：双击 → 页面一键接入 → 正常使用 Codex。复用已有配置事务，新增 `PreserveModel` 选项，不替换客户端模型。旧入口默认行为不变。原项目可能自动接入，而新记录器首次要求点击页面按钮，避免启动即修改配置或开始记录用户内容。

## 新增功能

- 根目录 Mac/Windows 双击入口、自动打开网页、一分钟一次性登录链接。
- 一键备份并接入、保留已有上游/认证设置/默认 profile/模型。
- 自动识别常见本机 SOCKS5 代理，或在页面选择直连/手动代理。
- 恢复原连接、恢复并退出、Ctrl+C 正常恢复、上次异常退出后的恢复提示。
- 单实例锁与重复启动重开面板。配置/认证类别被外部更改时阻止转发。
- 强制模型改写保持可选且默认关闭；模型别名没有恢复。

## 已实际执行

本机全量：18 个包、285 个顶层测试、180 个子测试通过；失败 0。

`go test -race -count=1 ./...`、`go vet ./...`、Windows 测试交叉编译均通过。交叉编译不是 Windows 运行验收；该提交的三平台运行结果以 GitHub Actions 为准。

隔离的真实 Chrome 浏览器自动化已经执行：

- automatic_login: `True`
- one_click_connect: `True`
- request_visible: `True`
- detail_opens: `True`
- restore_and_exit: `True`
- page_exceptions: `0`
- version: `0.4.0-oneclick`
- startup_did_not_modify_codex: `True`
- second_launch_reopens: `True`
- original_config_restored_byte_exact: `True`
- synthetic_upstream_calls: `1`
- real_upstream_calls: `0`
- server_left_running: `False`

只使用临时 Codex 配置、临时 Chrome 用户目录和回环模拟上游；没有修改开发机真实 Codex 配置、登录、系统代理或证书。浏览器验证覆盖自动登录、接入按钮、请求列表、详情、恢复并退出；没有浏览器脚本异常。

复现命令：

```sh
go test -race -count=1 ./...
go vet ./...
go build -trimpath -o bin/ccodex-request-recorder ./cmd/ccodex-request-recorder
python3 scripts/smoke-oneclick.py
```

`smoke-oneclick.py` 在存在 Chrome 的 macOS 使用独立无头浏览器，否则执行 HTTP 级检查。生成报告、截图和所有临时运行数据保存在 `.local/validation/`，不发布到源码仓库。真实账号兼容性不在本轮验收范围。
