# Changelog

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
