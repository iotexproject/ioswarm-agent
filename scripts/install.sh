#!/usr/bin/env bash
set -euo pipefail

# IOSwarm Agent Installer
# Usage: curl -sSL https://ioswarm.io/install.sh | bash

REPO="machinefi/ioswarm-agent"
BINARY="ioswarm-agent"
INSTALL_DIR="/usr/local/bin"
CONFIG_DIR="/etc/ioswarm"
SERVICE_NAME="ioswarm-agent"

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
CYAN='\033[0;36m'
NC='\033[0m'

log()   { echo -e "${GREEN}[ioswarm]${NC} $1"; }
warn()  { echo -e "${YELLOW}[ioswarm]${NC} $1"; }
error() { echo -e "${RED}[ioswarm]${NC} $1" >&2; }

# --- Detect OS and architecture ---
detect_platform() {
    OS=$(uname -s | tr '[:upper:]' '[:lower:]')
    ARCH=$(uname -m)

    case "$OS" in
        linux)  OS="linux" ;;
        darwin) OS="darwin" ;;
        *)      error "Unsupported OS: $OS"; exit 1 ;;
    esac

    case "$ARCH" in
        x86_64|amd64)   ARCH="amd64" ;;
        aarch64|arm64)  ARCH="arm64" ;;
        *)              error "Unsupported architecture: $ARCH"; exit 1 ;;
    esac

    log "Detected platform: ${OS}/${ARCH}"
}

# --- Get latest release version ---
get_latest_version() {
    VERSION=$(curl -sSL "https://api.github.com/repos/${REPO}/releases/latest" \
        | grep '"tag_name"' | head -1 | sed -E 's/.*"([^"]+)".*/\1/')

    if [ -z "$VERSION" ]; then
        error "Failed to fetch latest version from GitHub"
        exit 1
    fi
    log "Latest version: ${VERSION}"
}

# --- Download and install binary ---
download_binary() {
    DOWNLOAD_URL="https://github.com/${REPO}/releases/download/${VERSION}/${BINARY}-${OS}-${ARCH}"
    TMP_FILE=$(mktemp)

    log "Downloading ${DOWNLOAD_URL}..."
    if ! curl -sSL -o "$TMP_FILE" "$DOWNLOAD_URL"; then
        error "Download failed. Check if the release exists for ${OS}/${ARCH}."
        rm -f "$TMP_FILE"
        exit 1
    fi

    chmod +x "$TMP_FILE"

    if [ -w "$INSTALL_DIR" ]; then
        mv "$TMP_FILE" "${INSTALL_DIR}/${BINARY}"
    else
        log "Requires sudo to install to ${INSTALL_DIR}"
        sudo mv "$TMP_FILE" "${INSTALL_DIR}/${BINARY}"
    fi

    log "Installed ${BINARY} to ${INSTALL_DIR}/${BINARY}"
}

# --- Interactive configuration ---
configure_agent() {
    echo ""
    echo -e "${CYAN}=== IOSwarm Agent Configuration ===${NC}"
    echo ""

    # Coordinator address
    read -rp "Coordinator address [127.0.0.1:14689]: " COORDINATOR
    COORDINATOR=${COORDINATOR:-"127.0.0.1:14689"}

    # Agent ID
    read -rp "Agent ID (e.g., lobster-001): " AGENT_ID
    if [ -z "$AGENT_ID" ]; then
        error "Agent ID is required"
        exit 1
    fi

    # API Key
    read -rp "API Key (iosw_...): " API_KEY
    if [ -z "$API_KEY" ]; then
        warn "No API key provided. Auth will be disabled."
    fi

    # Region
    read -rp "Region [default]: " REGION
    REGION=${REGION:-"default"}

    # Wallet
    read -rp "IOTX Wallet address (for rewards, optional): " WALLET

    echo ""
    log "Configuration:"
    log "  Coordinator: ${COORDINATOR}"
    log "  Agent ID:    ${AGENT_ID}"
    log "  Region:      ${REGION}"
    log "  Wallet:      ${WALLET:-<none>}"
    log "  Auth:        $([ -n "${API_KEY}" ] && echo 'enabled' || echo 'disabled')"
}

# --- Write config file ---
write_config() {
    if [ -w "$(dirname "$CONFIG_DIR")" ]; then
        mkdir -p "$CONFIG_DIR"
    else
        sudo mkdir -p "$CONFIG_DIR"
    fi

    local ENV_FILE="${CONFIG_DIR}/agent.env"
    local ENV_CONTENT="IOSWARM_COORDINATOR=${COORDINATOR}
IOSWARM_AGENT_ID=${AGENT_ID}
IOSWARM_API_KEY=${API_KEY}
IOSWARM_WALLET=${WALLET}
IOSWARM_REGION=${REGION}"

    if [ -w "$CONFIG_DIR" ] 2>/dev/null; then
        echo "$ENV_CONTENT" > "$ENV_FILE"
        chmod 600 "$ENV_FILE"
    else
        echo "$ENV_CONTENT" | sudo tee "$ENV_FILE" > /dev/null
        sudo chmod 600 "$ENV_FILE"
    fi

    log "Config written to ${ENV_FILE} (mode 600)"
}

# --- Install systemd service (Linux only) ---
install_systemd_service() {
    if [ "$OS" != "linux" ]; then
        return 1
    fi

    if ! command -v systemctl &>/dev/null; then
        warn "systemctl not found, skipping service installation"
        return 1
    fi

    local SERVICE_FILE="/etc/systemd/system/${SERVICE_NAME}.service"
    local SERVICE_CONTENT="[Unit]
Description=IOSwarm Agent
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
EnvironmentFile=${CONFIG_DIR}/agent.env
ExecStart=${INSTALL_DIR}/${BINARY} \\
    --coordinator=\${IOSWARM_COORDINATOR} \\
    --agent-id=\${IOSWARM_AGENT_ID} \\
    --api-key=\${IOSWARM_API_KEY} \\
    --wallet=\${IOSWARM_WALLET} \\
    --region=\${IOSWARM_REGION}
Restart=always
RestartSec=5
LimitNOFILE=65536

[Install]
WantedBy=multi-user.target"

    echo "$SERVICE_CONTENT" | sudo tee "$SERVICE_FILE" > /dev/null
    sudo systemctl daemon-reload
    sudo systemctl enable "$SERVICE_NAME"
    sudo systemctl start "$SERVICE_NAME"

    log "systemd service installed and started"
    return 0
}

# --- Print manual start instructions (macOS fallback) ---
print_manual_instructions() {
    echo ""
    warn "systemd not available on this platform."
    echo ""
    log "To start the agent manually:"
    echo ""
    echo "  source ${CONFIG_DIR}/agent.env"
    echo "  ${BINARY} \\"
    echo "    --coordinator=\${IOSWARM_COORDINATOR} \\"
    echo "    --agent-id=\${IOSWARM_AGENT_ID} \\"
    echo "    --api-key=\${IOSWARM_API_KEY} \\"
    echo "    --wallet=\${IOSWARM_WALLET} \\"
    echo "    --region=\${IOSWARM_REGION}"
    echo ""
}

# --- Main ---
main() {
    echo ""
    echo -e "${CYAN}╔══════════════════════════════════╗${NC}"
    echo -e "${CYAN}║     IOSwarm Agent Installer      ║${NC}"
    echo -e "${CYAN}╚══════════════════════════════════╝${NC}"
    echo ""

    detect_platform
    get_latest_version
    download_binary
    configure_agent
    write_config

    if ! install_systemd_service; then
        print_manual_instructions
    fi

    echo ""
    echo -e "${GREEN}╔══════════════════════════════════╗${NC}"
    echo -e "${GREEN}║   Installation complete!         ║${NC}"
    echo -e "${GREEN}╚══════════════════════════════════╝${NC}"
    echo ""
    log "Agent ID: ${AGENT_ID}"
    log "Binary:   ${INSTALL_DIR}/${BINARY}"
    log "Config:   ${CONFIG_DIR}/agent.env"

    if [ "$OS" = "linux" ] && command -v systemctl &>/dev/null; then
        log "Status:   sudo systemctl status ${SERVICE_NAME}"
        log "Logs:     sudo journalctl -u ${SERVICE_NAME} -f"
    fi
    echo ""
}

main "$@"
