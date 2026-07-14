# Changelog

## v0.3.1 (2026-07)

### 新增
- Jackett/Prowlarr 索引器集成：库管理、激活/停用、连接测试、批量操作
- 搜索结果来源标签（本地/Jackett）、冲突索引器标注
- 搜索列表底部分页栏：页码跳转、上一页/下一页、总数显示
- 可配置搜索分页大小（设置页，10–500，默认 50）
- RSS 订阅支持 Jackett-only 索引器搜索

### 优化
- 搜索引擎支持多页翻页（Page 参数传递到 YAML 模板）
- Jackett HTTP 客户端代理支持
- 搜索结果日期格式统一含年份：`2006-01-02 15:04`
- 刷新按钮清除 sessionStorage 防止切页回访残留
- 分页栏双语支持（中/英翻译键）

### 修复
- Jackett 搜索超时（proxy 未配置导致直连超时）
- Jackett-only RSS 订阅地址过滤失效（`jackett:` 前缀未剥离）
- 设置页布局重组：系统/下载/搜索/订阅与通知/Jackett 五组 fieldset
- 移除冗余的访问域名和界面语言配置（侧栏已有语言切换）
- Dockerfile 引用不存在的 config.example.toml

## v0.3.0 (2026-07)

### 新增
- Web 文件管理：新建/重命名/删除/移动，自定义弹窗交互
- 订阅管理整合缓存库：内嵌查看资源名、清空、折叠展开
- 侧栏语言切换（CN/EN）、关于页面
- 全局操作日志：FS/索引器/订阅/设置全覆盖
- 磁力链接 dn= 参数保留，缓存显示资源名而非 hash
- 订阅立即执行为 AJAX 免刷新，3 秒后自动刷新
- 顶栏公告区，状态消息移入 topbar

### 优化
- pageData 性能：连接检查缓存 60s，FS/设置页跳过任务加载
- 面包屑条目缓存（getEntryCached）
- 设置日志仅记录实际变更

### 清理
- 移除不生效的"跳过去重"（disable_cache）
- 删除失效索引器（kickasstorrent、skytorrents、dmhy_jackett）
- 删除空 webdav/ 目录、冗余 rsshub.go、/tasks 路由

## v0.2.3

### 新增
- 统一 config.toml 配置方案
- Linux systemd 安装脚本

## v0.2.2

### 修复
- 适配 elevengo 上游变更
- 新增 savepath 可选字段
