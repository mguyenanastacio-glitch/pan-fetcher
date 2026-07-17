#!/usr/bin/env bash
set -Eeuo pipefail

# pan-fetcher Docker one-click setup

APP_DIR="${HOME}/pan-fetcher"
COMPOSE_URL="https://raw.githubusercontent.com/mguyenanastacio-glitch/pan-fetcher/master/docker-compose.yml"

echo "=== pan-fetcher Docker Setup ==="
echo ""

# Create app directory
mkdir -p "$APP_DIR"
cd "$APP_DIR"

# Download docker-compose.yml if missing
if [ ! -f docker-compose.yml ]; then
  echo "→ Downloading docker-compose.yml ..."
  curl -fsSL "$COMPOSE_URL" -o docker-compose.yml
else
  echo "→ docker-compose.yml already exists, skipping download."
fi

# Create default config if missing
if [ ! -f config.toml ]; then
  echo "→ Creating default config.toml ..."
  cat > config.toml << 'EOF'
[server]
port = 8115

[notify]
# wework_webhook = "https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=xxx"

[proxy]
# http = "http://127.0.0.1:7890"
EOF
else
  echo "→ config.toml already exists, keeping it."
fi

# Pull latest image and start
echo "→ Pulling latest image and starting ..."
docker compose pull 2>/dev/null || true
docker compose up -d

echo ""
echo "=== Done ==="
echo "Open http://localhost:8115 in your browser."
echo "Login via Cookies or QR code in Settings."
echo ""
echo "Useful commands:"
echo "  docker compose logs -f     # View logs"
echo "  docker compose restart     # Restart"
echo "  docker compose down        # Stop"
echo "  docker compose pull        # Update image"
