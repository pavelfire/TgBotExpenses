#!/usr/bin/env bash
set -euo pipefail

if [[ $# -lt 1 || $# -gt 2 ]]; then
  echo "Usage: $0 <user@host> [remote_dir]"
  echo "Example: $0 root@1.2.3.4 /opt/tg-expenses-bot"
  exit 1
fi

REMOTE_HOST="$1"
REMOTE_DIR="${2:-/opt/tg-expenses-bot}"

echo "Deploying to ${REMOTE_HOST}:${REMOTE_DIR}"

ssh "${REMOTE_HOST}" 'bash -s' <<'EOF'
set -euo pipefail

if command -v docker >/dev/null 2>&1 && docker compose version >/dev/null 2>&1; then
  echo "Docker and Compose already installed."
  exit 0
fi

if [[ "${EUID}" -ne 0 ]]; then
  if command -v sudo >/dev/null 2>&1; then
    SUDO="sudo"
  else
    echo "Need root privileges (run as root or install sudo)."
    exit 1
  fi
else
  SUDO=""
fi

if [[ -f /etc/debian_version ]]; then
  $SUDO apt-get update
  $SUDO apt-get install -y ca-certificates curl gnupg
  $SUDO install -m 0755 -d /etc/apt/keyrings
  if [[ ! -f /etc/apt/keyrings/docker.gpg ]]; then
    curl -fsSL https://download.docker.com/linux/ubuntu/gpg | $SUDO gpg --dearmor -o /etc/apt/keyrings/docker.gpg
    $SUDO chmod a+r /etc/apt/keyrings/docker.gpg
  fi
  if [[ -f /etc/os-release ]]; then
    . /etc/os-release
  fi
  CODENAME="${VERSION_CODENAME:-}"
  if [[ -z "${CODENAME}" ]]; then
    CODENAME="jammy"
  fi
  ARCH="$(dpkg --print-architecture)"
  echo "deb [arch=${ARCH} signed-by=/etc/apt/keyrings/docker.gpg] https://download.docker.com/linux/ubuntu ${CODENAME} stable" | $SUDO tee /etc/apt/sources.list.d/docker.list >/dev/null
  $SUDO apt-get update
  $SUDO apt-get install -y docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin
  $SUDO systemctl enable docker --now || true
else
  echo "Auto-install supported for Debian/Ubuntu only. Install Docker manually."
  exit 1
fi
EOF

tar --exclude=".git" \
    --exclude=".env" \
    --exclude="tgbotexpenses" \
    --exclude=".DS_Store" \
    -czf - . | ssh "${REMOTE_HOST}" "mkdir -p '${REMOTE_DIR}' && tar -xzf - -C '${REMOTE_DIR}'"

ssh "${REMOTE_HOST}" "cd '${REMOTE_DIR}' && if [ ! -f .env ]; then cp .env.docker.example .env; fi"

ssh "${REMOTE_HOST}" "cd '${REMOTE_DIR}' && docker compose pull && docker compose up -d --build"

echo "Done. If first deploy, edit ${REMOTE_DIR}/.env and set BOT_TOKEN."
