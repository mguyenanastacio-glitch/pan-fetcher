# pan-fetcher Docker one-click setup (Windows PowerShell)
# Run: iwr -useb https://raw.githubusercontent.com/mguyenanastacio-glitch/pan-fetcher/master/scripts/docker-setup.ps1 | iex

$ErrorActionPreference = "Stop"
$AppDir = "$env:USERPROFILE\pan-fetcher"
$ComposeUrl = "https://raw.githubusercontent.com/mguyenanastacio-glitch/pan-fetcher/master/docker-compose.yml"

Write-Host "=== pan-fetcher Docker Setup ===" -ForegroundColor Cyan
Write-Host ""

# Create app directory
New-Item -ItemType Directory -Force -Path $AppDir | Out-Null
Set-Location $AppDir

# Download docker-compose.yml if missing
if (-not (Test-Path docker-compose.yml)) {
  Write-Host "→ Downloading docker-compose.yml ..."
  Invoke-WebRequest -Uri $ComposeUrl -OutFile docker-compose.yml
} else {
  Write-Host "→ docker-compose.yml already exists, skipping."
}

# Create default config if missing
if (-not (Test-Path config.toml)) {
  Write-Host "→ Creating default config.toml ..."
  @"
[server]
port = 8115

[notify]
# wework_webhook = "https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=xxx"

[proxy]
# http = "http://127.0.0.1:7890"
"@ | Out-File -FilePath config.toml -Encoding UTF8
} else {
  Write-Host "→ config.toml already exists, keeping it."
}

# Pull and start
Write-Host "→ Pulling latest image and starting ..."
docker compose pull 2>$null
docker compose up -d

Write-Host ""
Write-Host "=== Done ===" -ForegroundColor Green
Write-Host "Open http://localhost:8115 in your browser."
Write-Host "Login via Cookies or QR code in Settings."
Write-Host ""
Write-Host "Useful commands:"
Write-Host "  docker compose logs -f     # View logs"
Write-Host "  docker compose restart     # Restart"
Write-Host "  docker compose down        # Stop"
Write-Host "  docker compose pull        # Update image"
