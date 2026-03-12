#!/bin/bash
set -euo pipefail

REPO="J132134/symphony"
BINARY="symphony"
INSTALL_DIR="${INSTALL_DIR:-$HOME/.local/bin}"
LOG_DIR="$HOME/Library/Logs/Symphony"
LAUNCH_AGENTS_DIR="$HOME/Library/LaunchAgents"
RAW_BASE="https://raw.githubusercontent.com/${REPO}/main"

WITH_LAUNCHAGENTS=0
for arg in "$@"; do
  case "$arg" in
    --with-launchagents) WITH_LAUNCHAGENTS=1 ;;
  esac
done

# Install binary
mkdir -p "${INSTALL_DIR}"
echo "Downloading symphony..."
curl -fsSL "https://github.com/${REPO}/releases/latest/download/${BINARY}-darwin-arm64" \
  -o "${INSTALL_DIR}/${BINARY}"
chmod 755 "${INSTALL_DIR}/${BINARY}"
xattr -c "${INSTALL_DIR}/${BINARY}"
codesign -s - --force "${INSTALL_DIR}/${BINARY}"
echo "Installed $("${INSTALL_DIR}/${BINARY}" version) → ${INSTALL_DIR}/${BINARY}"

# Add INSTALL_DIR to PATH if needed
if [[ ":$PATH:" != *":${INSTALL_DIR}:"* ]]; then
  SHELL_RC="$HOME/.zshrc"
  [[ "${SHELL:-}" == */bash ]] && SHELL_RC="$HOME/.bash_profile"
  echo "export PATH=\"${INSTALL_DIR}:\$PATH\"" >> "${SHELL_RC}"
  echo "Added ${INSTALL_DIR} to PATH in ${SHELL_RC} — run: source ${SHELL_RC}"
fi

# Create default config if not present
CONFIG_DIR="$HOME/.config/symphony"
CONFIG_FILE="${CONFIG_DIR}/config.yaml"
if [[ ! -f "${CONFIG_FILE}" ]]; then
  mkdir -p "${CONFIG_DIR}"
  curl -fsSL "${RAW_BASE}/scripts/config.yaml" -o "${CONFIG_FILE}"
  echo "Created config template → ${CONFIG_FILE}"
  echo "  Edit projects list, then run: symphony daemon"
fi

# LaunchAgents (optional)
if [[ "${WITH_LAUNCHAGENTS}" == "1" ]]; then
  if [[ -z "${LINEAR_API_KEY:-}" ]]; then
    echo "Warning: LINEAR_API_KEY not set — update ~/.config/symphony/config.yaml to add your key"
  fi
  mkdir -p "${LAUNCH_AGENTS_DIR}" "${LOG_DIR}"
  plist="com.symphony.daemon"
  curl -fsSL "${RAW_BASE}/scripts/${plist}.plist" \
    | sed -e "s|__HOME__|${HOME}|g" \
          -e "s|__LOG_DIR__|${LOG_DIR}|g" \
          -e "s|__LINEAR_API_KEY__|${LINEAR_API_KEY:-}|g" \
    > "${LAUNCH_AGENTS_DIR}/${plist}.plist"
  launchctl unload "${LAUNCH_AGENTS_DIR}/${plist}.plist" 2>/dev/null || true
  launchctl load "${LAUNCH_AGENTS_DIR}/${plist}.plist" 2>/dev/null || true
  echo "LaunchAgents installed. Status: launchctl list | grep symphony"
fi
