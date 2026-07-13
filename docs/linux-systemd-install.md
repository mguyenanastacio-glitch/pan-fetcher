# Linux systemd 安装指南

## 自动安装

```bash
sudo bash scripts/install-release.sh install
```

脚本自动完成：
- 下载/更新 `pan-fetcher` 二进制到 `/usr/local/bin/`
- 创建配置目录 `/var/lib/pan-fetcher/`
- 注册并启用 systemd 服务

## 手动配置

```bash
# 创建配置
sudo mkdir -p /var/lib/pan-fetcher
sudo cp config.toml /var/lib/pan-fetcher/
sudo cp .cookies /var/lib/pan-fetcher/
sudo cp rss.json /var/lib/pan-fetcher/

# 设置权限
sudo chown -R pan-fetcher:pan-fetcher /var/lib/pan-fetcher
```

## 服务管理

```bash
sudo systemctl start pan-fetcher
sudo systemctl stop pan-fetcher
sudo systemctl restart pan-fetcher
sudo systemctl status pan-fetcher
sudo journalctl -u pan-fetcher -f
```

## 其他命令

```bash
sudo bash scripts/install-release.sh update   # 更新二进制
sudo bash scripts/install-release.sh uninstall # 卸载（保留数据）
sudo bash scripts/install-release.sh purge     # 完全清除
```
