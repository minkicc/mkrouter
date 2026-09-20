# MKRouter

English | [简体中文](README.zh-CN.md)

MKRouter is a cross-platform desktop application for intelligently routing local AI model requests across multiple upstream channels. Codex and other OpenAI-compatible clients connect to one stable local endpoint while MKRouter balances cost, reliability, model availability, priorities, and failover behavior. Failed requests can be retried or routed to another available channel automatically.

### The problem MKRouter solves

Many AI users have several channels or accounts for the same model, but each one has a different price, stability profile, model catalog, and remaining token allowance. MKRouter turns those channels into one manageable pool:

- Prefer a lower-cost healthy channel for routine requests.
- Keep more reliable channels available as fallbacks for production or long-running tasks.
- Separate channels into groups such as low-cost, premium, and emergency.
- Map one client model name to the best upstream model available on each channel.
- Recover automatically when a channel returns an error, times out, or becomes unhealthy.

The result is one endpoint for clients, with a routing policy that can reduce avoidable spend without sacrificing resilience.

### A small-team AI relay on your LAN

Enable LAN access and let a small team share one self-hosted AI relay on a trusted network. Teammates keep using the same OpenAI-compatible Base URL while MKRouter centralizes upstream credentials, routing rules, health checks, and local usage records. Create separate local access tokens for different people or tools, and keep token authentication enabled whenever LAN access is turned on.

## Features

- Graphical management for channels, groups, priorities, model mappings, and access tokens
- Channels support no auth, Bearer/custom-header/query API keys, and Codex browser OAuth, `auth.json` / AT, RT, and PAT credentials
- Codex OAuth credentials support automatic rotation, manual refresh, and account-scoped model discovery, with account and expiry status shown in channel management
- Cost-aware channel ordering with configurable price, priority, groups, and weights
- Channel health checks, retries, cooldowns, and automatic failover
- Independent channel entries for accounts or tokens with different quotas and limits
- Client model names can be mapped to different upstream model names
- OpenAI-compatible `/v1/chat/completions`, `/v1/responses`, and `/v1/models` endpoints
- Multiple local access tokens with persistent request and token usage counters
- Local usage history with a default limit of 500 records
- Optional LAN access for a small, self-hosted team relay
- Simplified Chinese and English interface
- Windows, macOS, and Linux support

## Screenshots

### Channel management

![MKRouter channel management](docs/screenshots/channels-en-US.png)

### Usage history

![MKRouter usage history](docs/screenshots/usage-en-US.png)

### Utilities

![MKRouter utilities](docs/screenshots/utilities-en-US.png)

## How It Works

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

Tauri provides the desktop interface and process management. The Go backend provides the OpenAI-compatible proxy, routing, health checks, and local usage history.

## Typical routing policy

```text
Routine requests  -> low-cost + healthy channels
Heavy workloads   -> channels with higher remaining allowance
Critical requests -> premium/reliable group
Any failure       -> retry, cooldown, then fail over
```

MKRouter does not require clients to know which provider or account is active. Add each credential or endpoint as a channel, then express the policy with price, priority, groups, model mappings, and health checks.

## Installation

Download the package for your platform from [GitHub Releases](https://github.com/minkicc/mkrouter/releases):

- Windows: `.exe` or `.msi`
- Windows portable edition: extract `portable.zip` and run `MKRouter.exe`
- macOS: `.dmg`
- Linux: `.AppImage`, `.deb`, or `.rpm`

Starting with v1.1.5, macOS DMGs and their bundled applications in published Releases are signed with Developer ID Application. Check the release notes for Apple notarization status; signing alone does not imply notarization. Windows builds remain unsigned and may display a security warning on first launch.

The Windows portable edition does not require installation, but unsigned executables may still trigger Microsoft Defender SmartScreen. Verify the download with the `.sha256` file included in the release.

## Quick Start

1. Open MKRouter and add your upstream channels on the **Channels** tab.
2. Enter each channel's model list, price, group, and priority. Use separate entries when accounts or tokens have different limits.
3. Copy the Base URL from the **Connect** tab. Generate an access token unless **No Token Required** is enabled.
4. Set the client Base URL to `http://127.0.0.1:8787/v1` and provide the generated access token.

A model mapping can target one channel or **All** channels. When **All** is selected, only channels whose available-model list contains the mapped upstream model are eligible.

When adding or editing a channel, the following authorization methods are available:

- **API Key**: send as `Authorization: Bearer`, raw `Authorization`, a custom header, or a query parameter.
- **No authorization**: for local services or trusted upstreams that explicitly require no credential.
- **Codex browser OAuth**: generate a PKCE authorization URL and paste the localhost callback URL after sign-in.
- **Codex auth.json / Access Token**: import either a complete JSON document or a raw AT.
- **Codex Refresh Token**: exchange and validate before save, then maintain rotated credentials automatically.
- **Codex PAT**: validate an `at-` Personal Access Token and load its account identity; PAT credentials are not refreshable.

Every Codex channel shares a single client version. At startup MKRouter synchronizes the latest stable release from the official Codex release feed and uses it for model discovery, health checks, and inference, so the upstream never rejects a request for advertising a stale version. A failed synchronization keeps the last working version. Pin a version with `codex_client_version` in `config.json` (for example `0.155.1`), or set `codex_version_auto_sync` to `false` to disable synchronization.

While a channel is cooling down, the Channels tab shows its countdown together with the reason it was taken out of rotation, such as an upstream status code or connection error. The reason clears after the window expires and a request succeeds.

## Development

Requirements:

- Node.js 20+
- Rust stable
- Go 1.23+
- The [Tauri v2 prerequisites](https://v2.tauri.app/start/prerequisites/) for your operating system

Install dependencies and start the development application:

```bash
npm install
npm run dev
```

Run the Go test suite:

```bash
cd backend
go test ./...
```

When changing the Windows backend name, icon, or version metadata, regenerate
the committed resource object on Windows:

```powershell
npm run resource:windows
```

Build packages for the current platform:

```bash
npm run build
```

Create the Windows portable package after a full build:

```powershell
npm run portable:windows
```

The backend build script creates binaries for all supported architectures by default. CI can request one target explicitly:

```bash
npm run backend:build -- aarch64-apple-darwin
```

Supported targets:

- `x86_64-pc-windows-msvc`
- `x86_64-apple-darwin`
- `aarch64-apple-darwin`
- `x86_64-unknown-linux-gnu`
- `aarch64-unknown-linux-gnu`

## Data and Security

Desktop application data is stored in the system application configuration directory under the identifier `cc.minki.router`. On Windows, it is usually located at:

```text
%APPDATA%\cc.minki.router\
```

`config.json` contains upstream API keys, Codex OAuth/PAT authorization, and local access tokens. `usage.json` contains local usage records. Do not commit or share these files.

MKRouter automatically migrates `config.json` and `usage.json` from the previous `cc.minki.mkswitch` application directory when upgrading.

MKRouter listens on `127.0.0.1` by default. When LAN access is enabled, keep token authentication enabled and only use the application on trusted networks. MKRouter does not upload configuration or usage records by itself; proxied requests are still sent to the upstream services you configure.

## Automated Builds

GitHub Actions builds Windows, macOS, and Linux packages on pushes, pull requests, and manual runs. Pushing a version tag such as `v1.0` creates a draft GitHub Release and uploads all generated packages automatically. Sign the macOS packages locally and replace the draft assets before publishing.

```bash
git tag v1.0
git push origin v1.0
```

To sign a downloaded macOS build on a Mac with a Developer ID identity:

```bash
APPLE_SIGNING_IDENTITY="Developer ID Application: Your Company (TEAM_ID)" \
  bash scripts/sign-macos-dmg.sh unsigned/MKRouter_1.1.5_aarch64.dmg dist/MKRouter_1.1.5_aarch64.dmg
```

The script signs the backend, app, and DMG and writes a `.sha256` file. Repeat for the Intel DMG. Notarization is a separate step requiring Apple notarization credentials.

## License

[MIT](LICENSE)
