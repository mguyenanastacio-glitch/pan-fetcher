# pan-fetcher

115 网盘 RSS 订阅下载管理器，支持多索引器聚合搜索、文件管理、离线任务监控。

*[English](#english)*

## 功能

- **📡 聚合搜索** — 12 个 BT 索引器，类 Cardigann YAML 驱动，支持分类筛选和排序
- **📋 RSS 订阅** — 自动定时抓取（默认 60 分钟），info hash 去重缓存，支持关键词过滤
- **📥 离线下载** — 磁力/ed2k/http 链接批量提交到 115 离线任务，实时状态监控
- **📂 文件管理** — Web 端新建/重命名/删除/移动 115 云文件，面包屑导航
- **🗄️ 缓存管理** — 订阅页内嵌缓存查看，显示资源名称，支持清空和折叠展开
- **📜 运行日志** — 120 条环形缓冲，3 秒自动刷新，全操作日志覆盖
- **⚙️ Web 管理** — 中英双语（侧栏切换），自定义弹窗，设置保存，服务自重启

## 安装

### 预编译二进制（推荐）

从 [Releases](https://github.com/mguyenanastacio-glitch/pan-fetcher/releases) 下载对应平台的最新版本：

| 平台 | 文件 |
|------|------|
| Linux amd64 | `pan-fetcher-vX.Y.Z-linux-amd64.tar.gz` |
| Linux arm64 | `pan-fetcher-vX.Y.Z-linux-arm64.tar.gz` |
| macOS amd64 | `pan-fetcher-vX.Y.Z-darwin-amd64.tar.gz` |
| macOS arm64 | `pan-fetcher-vX.Y.Z-darwin-arm64.tar.gz` |
| Windows amd64 | `pan-fetcher-vX.Y.Z-windows-amd64.zip` |

解压后将二进制放到 `PATH` 目录下即可。

### Linux 一键安装

```bash
curl -fsSL https://raw.githubusercontent.com/mguyenanastacio-glitch/pan-fetcher/master/scripts/install-release.sh | sudo bash
```

自动完成：下载最新版 → 安装到 `/usr/local/bin/` → 创建数据目录 → 注册 systemd 服务。

更新或卸载：

```bash
sudo bash scripts/install-release.sh update     # 更新到最新版
sudo bash scripts/install-release.sh uninstall  # 卸载（保留数据）
sudo bash scripts/install-release.sh purge      # 完全清除
```

### 从源码编译（需 Go 1.23+）

```bash
git clone https://github.com/mguyenanastacio-glitch/pan-fetcher.git
cd pan-fetcher
go build -o pan-fetcher .

# 启动 Web 服务（默认端口 8115）
./pan-fetcher server
```

Windows 下编译产物为 `pan-fetcher.exe`，启动命令相同。

## 配置

启动后浏览器打开 `http://localhost:8115`，进入设置页粘贴 115 Cookies，填写其他参数后保存即可。配置文件详见 [config-files.md](docs/config-files.md)。

## 页面导航

| 页面 | 路由 | 功能 |
|------|------|------|
| 离线下载 | `/` | 提交磁力任务，查看离线任务状态，清理任务 |
| 索引器 | `/indexers` | 激活/停用/测试索引器，编辑 YAML 定义，站点登录 |
| 文件管理 | `/fs` | 浏览 115 目录，新建/重命名/删除/移动文件和文件夹 |
| 订阅管理 | `/subs` | RSS 订阅增删改，立即执行，缓存查看与清空 |
| 运行日志 | `/log` | 实时日志（3 秒轮询），自动滚动 |
| 设置 | `/settings` | Cookies 配置，参数调整，代理，服务重启 |
| 关于 | `/about` | 版本和项目信息 |

## CLI 命令

| 命令 | 说明 |
|------|------|
| `pan-fetcher server` | 启动 Web 管理面板 |
| `pan-fetcher magnet --link "magnet:?...` | 添加磁力任务 |
| `pan-fetcher fs ls [dir]` | 列出目录 |
| `pan-fetcher fs mkdir <path>` | 创建目录 |
| `pan-fetcher fs rename <path> <name>` | 重命名 |
| `pan-fetcher fs mv <src...> <dst>` | 移动 |
| `pan-fetcher fs rm <path...>` | 删除 |
| `pan-fetcher fs shell` | 交互式 Shell（Tab 补全） |

## 技术栈

Go 1.23 + SQLite + 单体 HTML 模板（内嵌 CSS/JS），无前端框架依赖。115 API 通过 elevengo 库调用。

## 致谢

本项目基于 [zhifengle/rss2cloud](https://github.com/zhifengle/rss2cloud)，索引引擎参考 [Prowlarr](https://github.com/Prowlarr/Prowlarr) 的 Cardigann 兼容设计。

---

## English {#english}

A 115 cloud storage RSS download manager with multi-indexer search, file management, and offline task monitoring.

### Features

- **📡 Aggregated Search** — 12 BT indexers, Cardigann-compatible YAML engine
- **📋 RSS Subscriptions** — Auto-fetch (default 60 min), info hash dedup, keyword filter
- **📥 Offline Download** — Magnet/ed2k/http batch submit to 115 cloud
- **📂 File Management** — Web UI for mkdir/rename/delete/move with breadcrumbs
- **🗄️ Cache Viewer** — Embedded cache view in subscriptions, with name display & clear
- **📜 Live Logs** — 120-line ring buffer, 3s auto-refresh
- **⚙️ Web Admin** — CN/EN bilingual, custom modals, settings with auto-restart

### Quick Start

**Pre-built binaries** (recommended): download from [Releases](https://github.com/mguyenanastacio-glitch/pan-fetcher/releases) for your platform.

**Linux one-liner:**

```bash
curl -fsSL https://raw.githubusercontent.com/mguyenanastacio-glitch/pan-fetcher/master/scripts/install-release.sh | sudo bash
```

**Build from source** (Go 1.23+):

```bash
git clone https://github.com/mguyenanastacio-glitch/pan-fetcher.git
cd pan-fetcher
go build -o pan-fetcher .
./pan-fetcher server   # http://localhost:8115
```

Paste 115 cookies in the Settings page to get started.

### Pages

| Page | Route | Description |
|------|-------|-------------|
| Downloads | `/` | Submit magnets, view & clear offline tasks |
| Indexers | `/indexers` | Activate/test/edit YAML indexer definitions |
| Files | `/fs` | Browse/manage 115 cloud files and folders |
| Subscriptions | `/subs` | CRUD RSS subs, run now, view/clear cache |
| Logs | `/log` | Real-time logs with auto-scroll |
| Settings | `/settings` | Cookies, proxy, parameters, restart |
| About | `/about` | Version and credits |

### Tech Stack

Go 1.23 + SQLite + single `html/template` (inline CSS/JS). 115 API via elevengo.

### Credits

Based on [zhifengle/rss2cloud](https://github.com/zhifengle/rss2cloud). Indexer engine inspired by [Prowlarr](https://github.com/Prowlarr/Prowlarr).
