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

tar --exclude=".git" \
    --exclude=".env" \
    --exclude="tgbotexpenses" \
    -czf - . | ssh "${REMOTE_HOST}" "mkdir -p '${REMOTE_DIR}' && tar -xzf - -C '${REMOTE_DIR}'"

ssh "${REMOTE_HOST}" "cd '${REMOTE_DIR}' && if [ ! -f .env ]; then cp .env.docker.example .env; fi"

ssh "${REMOTE_HOST}" "cd '${REMOTE_DIR}' && docker compose pull && docker compose up -d --build"

echo "Done. If first deploy, edit ${REMOTE_DIR}/.env and set BOT_TOKEN."
