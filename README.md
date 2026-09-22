# Codex 请求记录工具

独立 HTTP/SSE 请求记录、上游模型标识对照、额度观测。**强制模型改写默认关闭，不使用模型别名，不自动重发请求。**

## 开始使用：只需三步

1. **双击项目根目录的 `start.command`（Mac）或 `start.cmd`（Windows）。** 浏览器会自动打开，无需复制令牌。
2. **点击「一键接入 Codex」。** 自动识别当前上游和认证方式、备份连接配置，保留原模型。联网方式默认尝试常见本机代理；高级连接选项中可选直连或手填代理。
3. **重启 Codex，新建会话，正常发送消息。** 回复结束后，面板会自动显示记录与模型证据。

不用手改 JSON/TOML，不用复制 API 地址，不用开两个网关终端。首次请先确保 Codex 不经过本工具时原本就能正常使用。

**退出：**点击面板「恢复并退出」，或在程序窗口按 Ctrl+C。正常退出会恢复原连接。关闭浏览器不会停止服务。进程异常中断时，再次双击启动，页面会提供恢复入口；配置被其他工具修改时会保留备份，不强行覆盖。

> GitHub 当前提供源码，尚未提供预编译 Release。源码首次启动在没有 `bin/ccodex-request-recorder` 时需要 Go 1.26 或兼容版本，脚本会自动编译；已经有编译产物时直接启动，不重复编译。开发者更新源码后应重新构建二进制。不要将旧采集器的启动脚本与根目录的新入口混用。

## 面板里有什么

| 目的 | 操作 |
| --- | --- |
| 查看请求和上游模型标识 | 正常使用 Codex，点击下方请求记录 |
| 开启可选模型改写 | 展开「可选：强制模型改写」，填写目标并应用；默认关闭 |
| 修改联网方式 | 展开「连接设置」，选择自动、直连或手动代理，再接入 |
| 只暂停记录 | 点击「暂停记录」；不会停止转发或关闭模型改写 |
| 不再使用 | 点击「恢复并退出」 |

模型信息是上游**宣告的标识**，不是服务器内部执行证明。默认脱敏模式仍可能保存提示词和回复；不要随意分享记录和面板链接。

## 高级与开发

[独立记录器完整说明](README.recorder.md) · [可靠转发网关](README.hardened.md) · [一键流程实现与验证](docs/one-click-validation.md)

手动独立模式仍保留，不会接管 Codex：

```sh
./bin/ccodex-request-recorder -config config/request-recorder.json
```

从源码更新并启动一键模式：

```sh
go build -trimpath -o bin/ccodex-request-recorder ./cmd/ccodex-request-recorder
./bin/ccodex-request-recorder setup
```

## 来源与边界

基于 [gylive/ccodex-sleep-state](https://github.com/gylive/ccodex-sleep-state) 的公开源码开发。保留 GPL-3.0 许可证和第三方声明。参考了原项目的一键启动、可恢复 Codex 配置事务、页面引导；新记录器不启用原采集器。此前说明与原项目功能保留在 [归档说明](docs/upstream-and-previous-readme.md)。

可靠转发网关和旧采集器仍是独立入口；本次简化的是新记录器及其 Codex 接入流程，并未宣称所有工具已经合并。系统代理、证书信任、登录令牌刷新均不由本工具接管。这里只记录主动接入本地端口的流量，不截获 ChatGPT App 的其他 HTTPS 连接。
