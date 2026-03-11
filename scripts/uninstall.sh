#!/usr/bin/env bash
set -euo pipefail

# IOSwarm Agent Uninstaller

BINARY="ioswarm-agent"
INSTALL_DIR="/usr/local/bin"
CONFIG_DIR="/etc/ioswarm"
SERVICE_NAME="ioswarm-agent"

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

log()   { echo -e "${GREEN}[ioswarm]${NC} $1"; }
warn()  { echo -e "${YELLOW}[ioswarm]${NC} $1"; }
error() { echo -e "${RED}[ioswarm]${NC} $1" >&2; }

# --- Stop and disable systemd service ---
remove_service() {
    if command -v systemctl &>/dev/null; then
        if systemctl is-active --quiet "$SERVICE_NAME" 2>/dev/null; then
            log "Stopping ${SERVICE_NAME}..."
            sudo systemctl stop "$SERVICE_NAME"
        fi
        if systemctl is-enabled --quiet "$SERVICE_NAME" 2>/dev/null; then
            log "Disabling ${SERVICE_NAME}..."
            sudo systemctl disable "$SERVICE_NAME"
        fi

        local SERVICE_FILE="/etc/systemd/system/${SERVICE_NAME}.service"
        if [ -f "$SERVICE_FILE" ]; then
            sudo rm -f "$SERVICE_FILE"
            sudo systemctl daemon-reload
            log "Removed service file"
        fi
    fi
}

# --- Remove binary ---
remove_binary() {
    local BIN_PATH="${INSTALL_DIR}/${BINARY}"
    if [ -f "$BIN_PATH" ]; then
        sudo rm -f "$BIN_PATH"
        log "Removed ${BIN_PATH}"
    else
        warn "Binary not found at ${BIN_PATH}"
    fi
}

# --- Remove config (with confirmation) ---
remove_config() {
    if [ -d "$CONFIG_DIR" ]; then
        echo ""
        warn "Config directory ${CONFIG_DIR} contains your API key."
        read -rp "Delete config directory? [y/N]: " CONFIRM
        if [[ "${CONFIRM}" =~ ^[Yy]$ ]]; then
            sudo rm -rf "$CONFIG_DIR"
            log "Removed ${CONFIG_DIR}"
        else
            warn "Kept ${CONFIG_DIR}"
        fi
    fi
}

# --- Main ---
main() {
    echo ""
    log "Uninstalling IOSwarm Agent..."
    echo ""

    remove_service
    remove_binary
    remove_config

    echo ""
    log "Uninstall complete."
    echo ""
}

main "$@"
