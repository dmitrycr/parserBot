#!/usr/bin/env bash
set -euo pipefail

APP_NAME="${APP_NAME:-parser-tgchat-bot}"
APP_DIR="${APP_DIR:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)}"
GO_VERSION="${GO_VERSION:-1.25.1}"
SERVICE_NAME="${SERVICE_NAME:-parser-tgchat-bot}"
BOT_BIN="$APP_DIR/bin/bot"
SERVICE_FILE="/etc/systemd/system/$SERVICE_NAME.service"

if [[ "$(uname -s)" != "Linux" ]]; then
  echo "This installer is intended for Linux servers."
  exit 1
fi

if ! command -v apt-get >/dev/null 2>&1; then
  echo "Only apt-based distributions are supported by this script."
  exit 1
fi

if [[ "${EUID:-$(id -u)}" -ne 0 ]]; then
  SUDO="sudo"
else
  SUDO=""
fi

require_cmd() {
  if ! command -v "$1" >/dev/null 2>&1; then
    echo "Required command not found after installation: $1"
    exit 1
  fi
}

version_ge() {
  local current="$1"
  local required="$2"
  [[ "$(printf '%s\n%s\n' "$required" "$current" | sort -V | head -n1)" == "$required" ]]
}

install_go() {
  local current=""
  if command -v go >/dev/null 2>&1; then
    current="$(go version | awk '{print $3}' | sed 's/^go//')"
    if version_ge "$current" "$GO_VERSION"; then
      echo "Go $current is already installed."
      return
    fi
    echo "Go $current is older than required $GO_VERSION. Installing Go $GO_VERSION."
  else
    echo "Installing Go $GO_VERSION."
  fi

  local arch
  case "$(uname -m)" in
    x86_64 | amd64) arch="amd64" ;;
    aarch64 | arm64) arch="arm64" ;;
    *)
      echo "Unsupported CPU architecture: $(uname -m)"
      exit 1
      ;;
  esac

  local tarball="/tmp/go${GO_VERSION}.linux-${arch}.tar.gz"
  curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-${arch}.tar.gz" -o "$tarball"
  $SUDO rm -rf /usr/local/go
  $SUDO tar -C /usr/local -xzf "$tarball"
}

echo "Installing system packages."
$SUDO apt-get update
$SUDO apt-get install -y ca-certificates curl git tar build-essential

install_go
export PATH="/usr/local/go/bin:$PATH"
require_cmd go

echo "Preparing project directories."
mkdir -p "$APP_DIR/bin" "$APP_DIR/data"

echo "Downloading Go modules."
cd "$APP_DIR"
go mod download

echo "Building bot binary."
go build -o "$BOT_BIN" ./cmd/bot

echo "Installing systemd service: $SERVICE_NAME."
$SUDO tee "$SERVICE_FILE" >/dev/null <<EOF
[Unit]
Description=$APP_NAME
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
WorkingDirectory=$APP_DIR
ExecStart=$BOT_BIN
Restart=always
RestartSec=5
EnvironmentFile=$APP_DIR/.env

[Install]
WantedBy=multi-user.target
EOF

$SUDO systemctl daemon-reload
$SUDO systemctl enable "$SERVICE_NAME"

echo
echo "Installation complete."
echo "Check that $APP_DIR/.env exists and contains production values."
echo "If Telegram session is already authorized, copy it to $APP_DIR/data/session.json."
echo "Start the service with:"
echo "  sudo systemctl start $SERVICE_NAME"
echo "View logs with:"
echo "  sudo journalctl -u $SERVICE_NAME -f"
