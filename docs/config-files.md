# 配置文件说明

pan-fetcher 使用多层配置，按优先级：CLI 参数 > config.toml > 默认值。

## config.toml

```toml
[auth]
cookies = "UID=xxx; CID=xxx; SEID=xxx; KID=xxx"

[server]
port = 8115

[p115]
chunk_delay = 2
chunk_size = 200
cooldown_min_ms = 1000
cooldown_max_ms = 1100

[proxy]
http = "http://127.0.0.1:7890"

[[rss]]
site = "mikanani.me"
name = "订阅名称"
url = "https://mikanani.me/RSS/Bangumi?bangumiId=2739"
filter = "简体内嵌"
cid = "123456"
savepath = "动画"
```

## rss.json

旧版 RSS 订阅格式（兼容保留），由 Web 订阅管理页面自动维护：

```json
{
  "127.0.0.1:8115": [
    {
      "name": "订阅名称",
      "url": "http://127.0.0.1:8115/rss/search?...",
      "cid": "3461973339503854771",
      "savepath": "子目录",
      "enabled": true
    }
  ]
}
```

## web-settings.json

Web 面板设置（自动维护）：

```json
{
  "lang": "zh",
  "chunk_size": 200,
  "chunk_delay": 2,
  "cooldown_min": 1000,
  "cooldown_max": 1100,
  "subs_interval": 60,
  "web_password": ""
}
```

## dedup-cache.json

去重缓存（自动维护），按订阅名存储 info hash，新增条目自动从 magnet `dn=` 提取资源名称。

## .cookies

115 Cookies 纯文本文件，由设置页或 CLI 管理。
