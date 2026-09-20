# MKRouter

[English](README.md) | 简体中文

MKRouter 是一个跨平台的本地 AI 请求智能路由桌面应用。Codex 或其他 OpenAI 兼容客户端只需连接一个固定的本地地址，MKRouter 会在多个上游渠道之间综合考虑价格、稳定性、模型可用性、优先级和故障转移策略。请求失败时可以自动重试、冷却异常渠道并切换到其他可用渠道。

### MKRouter 解决的核心问题

很多 AI 用户同时拥有多个渠道或账号，但它们的价格、稳定性、可用模型和剩余 Token 额度各不相同。MKRouter 把这些渠道组织成一个可调度的资源池：

- 日常请求优先使用价格更低且健康的渠道。
- 将更稳定的渠道保留给生产任务、长上下文或关键请求。
- 按低价、专业、应急等用途建立分组。
- 使用统一的客户端模型名，并为不同渠道映射到最合适的上游模型。
- 当渠道报错、超时或健康状态恶化时自动重试和故障转移。

这样，客户端只需要连接一个地址，就能在尽量降低不必要开支的同时保持可用性和韧性。

### 局域网小团队中转服务

开启局域网访问后，可以在可信内网中快速自建一个小团队共享的 AI 中转服务。团队成员使用同一个 OpenAI 兼容 Base URL，MKRouter 统一管理上游凭据、调度规则、健康检查和本地使用记录。可以为不同成员或工具生成独立的本地访问 Token；开启局域网访问时，请始终保留 Token 鉴权，并只在可信网络中使用。

## 功能

- 图形化管理渠道、分组、优先级、模型映射和访问 Token
- 渠道支持免授权、Bearer / 自定义 Header / Query API Key，以及 Codex 浏览器 OAuth、`auth.json` / AT、RT 和 PAT
- Codex OAuth 凭据支持自动轮换、手动刷新和按账号发现可用模型，并在渠道状态中展示账号、过期时间和可刷新状态
- 支持按价格、优先级、分组和权重进行成本导向的渠道排序
- 支持渠道健康检查、失败重试、冷却和自动切换
- 可将不同额度或限制的账号 / Token 作为独立渠道管理
- 支持将客户端模型名映射为不同渠道的上游模型名
- 支持 `/v1/chat/completions`、`/v1/responses` 和 `/v1/models`
- 支持多个本地访问 Token，并持久化请求数与 Token 用量
- 本地保存使用记录，默认最多保留 500 条
- 支持局域网访问，适合快速自建小团队中转服务
- 支持简体中文和英文
- 支持 Windows、macOS 和 Linux

## 界面预览

### 渠道管理

![MKRouter 渠道管理](docs/screenshots/channels-zh-CN.png)

### 使用记录

![MKRouter 使用记录](docs/screenshots/usage-zh-CN.png)

### 实用工具

![MKRouter 实用工具](docs/screenshots/utilities-zh-CN.png)

## 工作方式

```text
Codex / OpenAI-compatible client
              |
              v
    http://127.0.0.1:8787/v1
              |
              v
         MKRouter
       /      |       \
  Channel A Channel B Channel C
```

Tauri 提供桌面界面和进程管理，Go 后端提供 OpenAI 兼容代理、路由、健康检查和本地使用记录。

## 典型调度策略

```text
日常请求   -> 低价 + 健康渠道
重度任务   -> 剩余额度更充足的渠道
关键请求   -> 专业 / 高稳定性分组
任何失败   -> 重试、冷却，再自动切换
```

客户端不需要知道当前使用的是哪家 Provider 或哪个账号。将每个凭据或接口添加为一个渠道，再通过价格、优先级、分组、模型映射和健康检查表达调度策略即可。

## 安装

从 [GitHub Releases](https://github.com/minkicc/mkrouter/releases) 下载对应平台的安装包：

- Windows：`.exe` 或 `.msi`
- Windows 绿色版：`portable.zip`，解压后运行 `MKRouter.exe`
- macOS：`.dmg`
- Linux：`.AppImage`、`.deb` 或 `.rpm`

从 v1.1.5 起，正式 Release 中的 macOS DMG 和内含应用使用 Developer ID Application 签名。Apple 公证状态请查看对应 Release 说明；签名本身不代表已公证。Windows 构建暂未代码签名，系统可能在首次启动时显示安全提示。

绿色版不需要安装，也不会写入程序目录以外的安装信息，但未签名的可执行文件仍可能触发 Windows SmartScreen。下载后可使用 Release 中的 `.sha256` 文件校验完整性。

## 快速使用

1. 打开 MKRouter，在「渠道」页添加你的多个上游渠道。
2. 为每个渠道填写模型列表、价格、分组和优先级；额度或限制不同的账号请分别添加。
3. 在「接入」页复制 Base URL；若未开启「无需 Token」，请生成一个访问 Token。
4. 将客户端的 Base URL 设置为 `http://127.0.0.1:8787/v1`，并填入访问 Token。

模型映射可以限定到某个渠道，也可以选择「所有」。选择「所有」时，只会匹配可用模型列表中包含该上游模型的渠道。

添加或编辑渠道时可选择以下授权方式：

- **API Key**：可发送为 `Authorization: Bearer`、原始 `Authorization`、自定义请求头或 Query 参数。
- **无需授权**：适用于本地服务或明确不要求凭据的可信上游。
- **Codex 浏览器 OAuth**：生成带 PKCE 的授权链接，登录后粘贴 localhost 回调链接。
- **Codex auth.json / Access Token**：支持完整 JSON 或原始 AT。
- **Codex Refresh Token**：保存前先兑换并校验，之后自动维护轮换令牌。
- **Codex PAT**：校验 `at-` Personal Access Token 并读取账号信息；PAT 本身不自动刷新。

所有 Codex 渠道共用同一个客户端版本：启动后会自动从 Codex 官方发布记录同步最新稳定版本，并用于模型发现、健康检查和真实推理请求，避免上游因版本过旧而拒绝请求。同步失败时会继续沿用上一次的可用版本，不影响请求转发。需要固定版本时，可在 `config.json` 中设置 `codex_client_version`（如 `0.155.1`）；设置 `codex_version_auto_sync` 为 `false` 可关闭自动同步。

渠道进入冷却时，「渠道」页会显示倒计时和触发原因（例如上游返回的状态码或连接错误），冷却结束并有一次成功请求后自动清除。

## 本地开发

环境要求：

- Node.js 20+
- Rust stable
- Go 1.23+
- 当前系统所需的 [Tauri v2 系统依赖](https://v2.tauri.app/start/prerequisites/)

安装依赖并启动开发版：

```bash
npm install
npm run dev
```

运行 Go 测试：

```bash
cd backend
go test ./...
```

修改 Windows 后端的名称、图标或版本信息后，需要在 Windows 上重新生成并提交资源文件：

```powershell
npm run resource:windows
```

构建当前平台安装包：

```bash
npm run build
```

完整构建后生成 Windows 绿色包：

```powershell
npm run portable:windows
```

构建脚本默认生成所有支持架构的 Go 后端。CI 也可以只生成指定架构：

```bash
npm run backend:build -- aarch64-apple-darwin
```

支持的 target：

- `x86_64-pc-windows-msvc`
- `x86_64-apple-darwin`
- `aarch64-apple-darwin`
- `x86_64-unknown-linux-gnu`
- `aarch64-unknown-linux-gnu`

## 数据与安全

桌面应用的数据保存在系统应用配置目录中。应用标识为 `cc.minki.router`；Windows 下通常位于：

```text
%APPDATA%\cc.minki.router\
```

`config.json` 包含渠道 API Key、Codex OAuth / PAT 授权和本地访问 Token，`usage.json` 包含本地使用记录。不要将这些文件提交到 Git 仓库或发送给他人。

从旧版本升级时，MKRouter 会自动从原来的 `cc.minki.mkswitch` 应用目录迁移 `config.json` 和 `usage.json`。

MKRouter 默认仅监听 `127.0.0.1`。开启「局域网访问」后，请务必启用 Token 验证，并只在可信网络中使用。MKRouter 不会主动上传配置或使用记录；代理请求仍会发送到你配置的上游服务。

## 自动构建

GitHub Actions 会在推送、Pull Request 和手动触发时构建 Windows、macOS 与 Linux 安装包。推送形如 `v1.0` 的版本标签时，会自动创建草稿 GitHub Release 并上传所有安装包。macOS 安装包需完成本地签名、替换草稿中的附件后再发布。

```bash
git tag v1.0
git push origin v1.0
```

在已安装 Developer ID 签名身份的 Mac 上，对下载的 macOS 构建签名：

```bash
APPLE_SIGNING_IDENTITY="Developer ID Application: Your Company (TEAM_ID)" \
  bash scripts/sign-macos-dmg.sh unsigned/MKRouter_1.1.5_aarch64.dmg dist/MKRouter_1.1.5_aarch64.dmg
```

脚本会依次签名后端、应用和 DMG，并生成 `.sha256` 文件。Intel DMG 也需执行一次。Apple 公证需另行配置公证凭据后进行。

## License

[MIT](LICENSE)
