# pan-fetcher · 115 网盘 RSS BT 下载订阅工具

<p align="center">
  <strong>聚合搜索 · 自动订阅 · 离线下载 · 文件管理 · 企业微信通知</strong>
</p>

<p align="center">
  <a href="https://github.com/mguyenanastacio-glitch/pan-fetcher/releases"><img src="https://img.shields.io/github/v/release/mguyenanastacio-glitch/pan-fetcher?style=flat-square" alt="release"></a>
  <a href="https://github.com/mguyenanastacio-glitch/pan-fetcher/blob/master/LICENSE"><img src="https://img.shields.io/github/license/mguyenanastacio-glitch/pan-fetcher?style=flat-square" alt="license"></a>
  <a href="https://golang.org"><img src="https://img.shields.io/badge/Go-1.23+-00ADD8?style=flat-square&logo=go" alt="go"></a>
</p>

---

自动追番 / 追剧工具。搜索 → 订阅 → 自动推送到 115 离线下载，Web 面板管理全流程。

[English](#english)

## ✨ 特性

<table>
<tr><td width="50%">

### 🔍 聚合搜索
12 个 BT 索引器 · Cardigann YAML 兼容 · 分类筛选 · 关键词实时过滤 · 搜索结果一键订阅

### 📋 RSS 订阅
定时抓取 · info hash 去重 · 关键词/正则过滤 · 启用/禁用开关 · 缓存管理

### 📥 离线下载
磁力 / ed2k / http 批量提交 · 下载中 / 失败 / 已完成分页 · 任务清理

</td><td width="50%">

### 📊 仪表盘
推送统计 · 订阅活跃度 · 索引器数量 · 缓存条目 · 运行时长 · 密码状态

### 🔔 通知推送
企业微信 Webhook · 任务/RSS/启动独立开关 · 一键测试

### ⚙️ 部署友好
Docker 一键部署 · HTTPS/TLS · Linux systemd · 5 平台预编译二进制

</td></tr>
</table>

## � 与 115 网盘的交互

pan-fetcher 通过 115 官方 API 操作你的网盘，登录方式有两种：

| 方式 | 说明 |
|------|------|
| **Cookies 登录** | 从浏览器复制 115 登录 Cookies，粘贴到设置页即可（推荐） |
| **扫码登录** | 设置页点击扫码，用 115 手机 App 扫描二维码登录 |

登录后可执行的操作：

```
搜索资源 → RSS 订阅 → 定时抓取 → 自动推送到 115 离线任务 → 文件管理
```

- **离线下载**：将磁力/ed2k/http 链接提交到 115 离线服务器，由 115 云端执行下载
- **文件管理**：浏览、新建、重命名、移动、复制、删除 115 网盘文件
- **订阅流程**：RSS 解析 → 提取 magnet / info hash → 去重检查 → `POST /add` 提交 → 115 调度下载

> 所有操作均通过 115 官方 API (`webapi.115.com` / `proapi.115.com`) 进行，不传输数据到第三方。

## �🚀 快速开始

### Docker（推荐）

```bash
mkdir pan-fetcher && cd pan-fetcher
wget https://raw.githubusercontent.com/mguyenanastacio-glitch/pan-fetcher/master/docker-compose.yml
wget https://raw.githubusercontent.com/mguyenanastacio-glitch/pan-fetcher/master/config.example.toml -O config.toml
# 编辑 config.toml 填入 115 cookies
mkdir -p data && docker-compose up -d
```

浏览器打开 `http://<IP>:8115`，进入设置页完成配置。

### Linux 一键脚本

```bash
curl -fsSL https://raw.githubusercontent.com/mguyenanastacio-glitch/pan-fetcher/master/scripts/install-release.sh | sudo bash
```

### 预编译二进制

从 [Releases](https://github.com/mguyenanastacio-glitch/pan-fetcher/releases) 下载对应平台文件，解压即可运行。

| Linux amd64 | Linux arm64 | macOS amd64 | macOS arm64 | Windows amd64 |
|:--:|:--:|:--:|:--:|:--:|
| `tar.gz` | `tar.gz` | `tar.gz` | `tar.gz` | `zip` |

### 从源码编译

```bash
git clone https://github.com/mguyenanastacio-glitch/pan-fetcher.git
cd pan-fetcher && go build -o pan-fetcher .
./pan-fetcher server
```

## 🖥️ 页面导航

| 页面 | 路由 | 说明 |
|------|------|------|
| 仪表盘 | `/` | 推送/订阅/索引器/缓存统计，运行时长 |
| 离线下载 | `/tasks` | 磁力提交，分页任务列表，清理 |
| 资源搜索 | `/search` | 跨站聚合搜索，关键词过滤 |
| 索引器 | `/indexers` | 激活/测试/编辑 YAML 定义 |
| 文件管理 | `/fs` | 115 目录浏览、新建、移动、复制 |
| 订阅管理 | `/subs` | RSS 增删改、手动执行、缓存查看 |
| 运行日志 | `/log` | 实时日志，自动刷新 |
| 设置 | `/settings` | Cookies、代理、域名、HTTPS、通知 |

## ⌨️ CLI 命令

```bash
pan-fetcher server                     # 启动 Web 面板
pan-fetcher magnet --link "magnet:..." # 添加磁力
pan-fetcher fs ls [dir]                # 列出目录
pan-fetcher fs shell                   # 交互式 Shell（Tab 补全）
```

完整命令：`ls`, `mkdir`, `rename`, `mv`, `rm`, `cp`, `stat`, `pwd`, `flatten`, `search-mv`

## 🔧 配置示例

```toml
# config.toml（可选，Web 设置页即可完成全部配置）
[server]
port = 8115
# domain = "pan.example.com"           # 域名访问
# cert_file = "/certs/fullchain.pem"   # HTTPS
# key_file = "/certs/privkey.pem"

[notify]
wework_webhook = "https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=xxx"
```

## 🛠️ 技术栈

Go 1.23 · SQLite · 单体 `html/template`（零前端依赖） · [elevengo](https://github.com/Nahuimi/elevengo) 115 API

## 🙏 致谢

- [zhifengle/rss2cloud](https://github.com/zhifengle/rss2cloud) — 项目原型
- [Prowlarr](https://github.com/Prowlarr/Prowlarr) — Cardigann 索引引擎参考
- [Nahuimi/elevengo](https://github.com/Nahuimi/elevengo) — 115 API 库

---

## English {#english}

Automated media downloader for 115 cloud storage. Search across BT indexers, subscribe to RSS feeds, auto-push to offline tasks — all managed via a clean Web UI.

### Features

- **📊 Dashboard** — Push count, active subs, indexers, cache stats, uptime
- **🔍 Aggregated Search** — 12 indexers via Cardigann-compatible YAML engine
- **📋 RSS Subscriptions** — Auto-fetch, info hash dedup, keyword filter
- **📥 Offline Download** — Magnet/ed2k/http batch submit with status tabs
- **📂 File Manager** — Browse, rename, move, copy, delete 115 cloud files
- **🔔 Notifications** — WeChat Work webhook with per-event toggles
- **🐳 Docker** — One-command deploy, image from ghcr.io
- **⚙️ Web Admin** — CN/EN bilingual, HTTPS, password auth, self-restart

### Quick Start

**Docker (recommended):**

```bash
mkdir pan-fetcher && cd pan-fetcher
wget https://raw.githubusercontent.com/mguyenanastacio-glitch/pan-fetcher/master/docker-compose.yml
wget https://raw.githubusercontent.com/mguyenanastacio-glitch/pan-fetcher/master/config.example.toml -O config.toml
mkdir -p data && docker-compose up -d
```

**Linux script:** `curl -fsSL https://.../install-release.sh | sudo bash`

**Binaries:** [GitHub Releases](https://github.com/mguyenanastacio-glitch/pan-fetcher/releases)

**From source:** `go build -o pan-fetcher . && ./pan-fetcher server`

### Tech Stack

Go 1.23 · SQLite · single `html/template` (zero frontend deps) · [elevengo](https://github.com/Nahuimi/elevengo)

### Credits

Based on [zhifengle/rss2cloud](https://github.com/zhifengle/rss2cloud). Indexer engine inspired by [Prowlarr](https://github.com/Prowlarr/Prowlarr).

### License

[MIT](LICENSE)