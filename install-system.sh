#!/usr/bin/env bash

set -euo pipefail

REPO="${GITHUB_REPO:-wdaglb/gpt-register}"
BINARY_NAME="${GO_REGISTER_BINARY_NAME:-go-register}"
TAG="${GO_REGISTER_TAG:-latest}"
WEBMAIL_SERVICE_NAME="go-register-webmail"
PIPELINE_SERVICE_NAME="gpt-register"
SERVICE_DIR="/etc/systemd/system"
WEBMAIL_ENV_FILE="/etc/default/go-register-webmail"
PIPELINE_ENV_FILE="/etc/default/gpt-register"
TMP_DIR="$(mktemp -d)"

log() {
  printf '[install-system] %s\n' "$*"
}

warn() {
  printf '[install-system][warn] %s\n' "$*" >&2
}

fail() {
  printf '[install-system][error] %s\n' "$*" >&2
  exit 1
}

cleanup() {
  rm -rf "${TMP_DIR}"
}

trap cleanup EXIT

write_file_if_missing() {
  local path="$1"
  local content="$2"

  if [[ -e "${path}" ]]; then
    warn "检测到已存在文件，跳过覆盖: ${path}"
    return
  fi

  mkdir -p "$(dirname "${path}")"
  printf '%s' "${content}" > "${path}"
}

write_root_file_if_missing() {
  local path="$1"
  local content="$2"
  local temp_file="${TMP_DIR}/$(basename "${path}").tmp"

  if run_as_root test -e "${path}"; then
    warn "检测到已存在文件，跳过覆盖: ${path}"
    return
  fi

  printf '%s' "${content}" > "${temp_file}"
  run_as_root install -m 0644 "${temp_file}" "${path}"
}

run_as_root() {
  # Why: systemd 单元文件必须写入 /etc/systemd/system，普通用户场景下统一经由 sudo 提权，
  # 避免脚本一半以当前用户执行、一半以 root 执行导致目录和属主混乱。
  if [[ "${EUID}" -eq 0 ]]; then
    "$@"
    return
  fi

  if ! command -v sudo >/dev/null 2>&1; then
    fail "未找到 sudo，无法安装 systemd 服务"
  fi

  sudo "$@"
}

resolve_target_user() {
  TARGET_USER="${SUDO_USER:-$(id -un)}"

  if command -v getent >/dev/null 2>&1; then
    TARGET_HOME="$(getent passwd "${TARGET_USER}" | cut -d: -f6)"
  fi

  if [[ -z "${TARGET_HOME:-}" ]]; then
    TARGET_HOME="$(eval echo "~${TARGET_USER}")"
  fi

  if [[ -z "${TARGET_HOME}" || ! -d "${TARGET_HOME}" ]]; then
    fail "无法确定目标用户 ${TARGET_USER} 的家目录"
  fi

  # Why: 这里与 install.sh 保持一致，统一把二进制、数据文件和 systemd WorkingDirectory 收口到同一目录。
  APP_DIR="${INSTALL_DIR:-${TARGET_HOME}/gpt-register}"
  WORK_DIR="${WORK_DIR:-${APP_DIR}}"
  WEBMAIL_HOST="${WEBMAIL_HOST:-127.0.0.1}"
  WEBMAIL_PORT="${WEBMAIL_PORT:-8030}"
  MAIL_API_BASE="${MAIL_API_BASE:-https://www.appleemail.top}"
  WEBMAIL_LEASE_TIMEOUT_SECONDS="${WEBMAIL_LEASE_TIMEOUT_SECONDS:-600}"
  PIPELINE_PROXY="${PIPELINE_PROXY:-http://127.0.0.1:7890}"
  PIPELINE_COUNT="${PIPELINE_COUNT:-5}"
  PIPELINE_WORKERS="${PIPELINE_WORKERS:-2}"
  PIPELINE_AUTHORIZE_WORKERS="${PIPELINE_AUTHORIZE_WORKERS:-2}"
  PIPELINE_MAILBOX="${PIPELINE_MAILBOX:-Junk}"
  PIPELINE_WEBMAIL_URL="${PIPELINE_WEBMAIL_URL:-http://${WEBMAIL_HOST}:${WEBMAIL_PORT}}"
}

resolve_platform() {
  local os_name
  local arch_name

  os_name="$(uname -s)"
  arch_name="$(uname -m)"

  if [[ "${os_name}" != "Linux" ]]; then
    fail "install-system.sh 仅支持 Linux systemd 环境，检测到系统: ${os_name}"
  fi

  if [[ ! -d /run/systemd/system ]]; then
    fail "当前环境未检测到 systemd"
  fi

  if ! command -v systemctl >/dev/null 2>&1; then
    fail "未找到 systemctl"
  fi

  case "${arch_name}" in
    x86_64|amd64) GOARCH="amd64" ;;
    arm64|aarch64) GOARCH="arm64" ;;
    *)
      fail "当前安装脚本暂不支持该架构: ${arch_name}"
      ;;
  esac

  GOOS="linux"
  ARCHIVE_EXT="tar.gz"
  ASSET_NAME="${BINARY_NAME}_${GOOS}_${GOARCH}.${ARCHIVE_EXT}"
}

resolve_download_url() {
  # Why: 默认追踪 latest，便于主分支发布后直接安装；需要锁版本时仍允许通过环境变量覆盖 tag。
  if [[ "${TAG}" == "latest" ]]; then
    DOWNLOAD_URL="https://github.com/${REPO}/releases/latest/download/${ASSET_NAME}"
    return
  fi

  DOWNLOAD_URL="https://github.com/${REPO}/releases/download/${TAG}/${ASSET_NAME}"
}

download_asset() {
  local target_file="${TMP_DIR}/${ASSET_NAME}"

  log "下载二进制包: ${DOWNLOAD_URL}"
  if command -v curl >/dev/null 2>&1; then
    curl -fL --retry 3 --connect-timeout 15 -o "${target_file}" "${DOWNLOAD_URL}"
  elif command -v wget >/dev/null 2>&1; then
    wget -O "${target_file}" "${DOWNLOAD_URL}"
  else
    fail "未找到 curl 或 wget，无法下载 GitHub Release 产物"
  fi

  ARCHIVE_PATH="${target_file}"
}

extract_binary() {
  log "解压二进制包"
  tar -xzf "${ARCHIVE_PATH}" -C "${TMP_DIR}"

  EXTRACTED_BINARY="${TMP_DIR}/${BINARY_NAME}"
  [[ -f "${EXTRACTED_BINARY}" ]] || fail "解压后未找到二进制文件: ${BINARY_NAME}"
}

install_binary() {
  local target_binary="${APP_DIR}/${BINARY_NAME}"
  mkdir -p "${APP_DIR}"
  if [[ -f "${target_binary}" ]]; then
    log "检测到已存在二进制，执行覆盖安装: ${target_binary}"
    rm -f "${target_binary}"
  fi
  install -m 0755 "${EXTRACTED_BINARY}" "${target_binary}"
  log "二进制已安装到: ${target_binary}"
}

prepare_runtime_files() {
  # Why: systemd 服务不会交互式创建运行目录，因此这里一次性补齐最小运行结构，保证 enable --now 后即可启动。
  mkdir -p "${WORK_DIR}/auth"
  touch "${WORK_DIR}/accounts.txt"
  touch "${WORK_DIR}/emails.txt"
  touch "${WORK_DIR}/user.txt"
  write_default_config
  write_system_env_files
  log "已初始化运行目录: ${WORK_DIR}"
}

write_default_config() {
  local config_path="${WORK_DIR}/.config.json"

  # Why: 即便服务场景主要通过 systemd 环境文件驱动，仍然生成一份 TUI 默认配置，
  # 方便用户后续在同一目录下手动运行程序或排查配置差异；若文件已存在则保留用户已有配置。
  write_file_if_missing "${config_path}" "$(cat <<EOF
{
  "mode": "pipeline",
  "web-mail-url": "${PIPELINE_WEBMAIL_URL}",
  "email": "",
  "password": "",
  "user-file": "user.txt",
  "auth-dir": "auth",
  "accounts-file": "accounts.txt",
  "proxy": "${PIPELINE_PROXY}",
  "mailbox": "${PIPELINE_MAILBOX}",
  "count": ${PIPELINE_COUNT},
  "workers": ${PIPELINE_WORKERS},
  "authorize-workers": ${PIPELINE_AUTHORIZE_WORKERS},
  "timeout": "4m0s",
  "otp-timeout": "1m30s",
  "poll-interval": "3s",
  "request-timeout": "20s"
}
EOF
)"
}

write_system_env_files() {
  # Why: systemd 场景把默认配置外置到 /etc/default，后续用户只改环境文件并重启服务即可生效，
  # 避免每次调参都重写 unit 文件。
  write_root_file_if_missing "${WEBMAIL_ENV_FILE}" "$(cat <<EOF
WEBMAIL_HOST=${WEBMAIL_HOST}
WEBMAIL_PORT=${WEBMAIL_PORT}
WEBMAIL_EMAILS_FILE=${WORK_DIR}/emails.txt
MAIL_API_BASE=${MAIL_API_BASE}
WEBMAIL_LEASE_TIMEOUT_SECONDS=${WEBMAIL_LEASE_TIMEOUT_SECONDS}
EOF
)"

  write_root_file_if_missing "${PIPELINE_ENV_FILE}" "$(cat <<EOF
PIPELINE_ACCOUNTS_FILE=${WORK_DIR}/accounts.txt
PIPELINE_AUTH_DIR=${WORK_DIR}/auth
PIPELINE_PROXY=${PIPELINE_PROXY}
PIPELINE_WEBMAIL_URL=${PIPELINE_WEBMAIL_URL}
PIPELINE_COUNT=${PIPELINE_COUNT}
PIPELINE_WORKERS=${PIPELINE_WORKERS}
PIPELINE_AUTHORIZE_WORKERS=${PIPELINE_AUTHORIZE_WORKERS}
PIPELINE_MAILBOX=${PIPELINE_MAILBOX}
PIPELINE_TIMEOUT=4m
PIPELINE_OTP_TIMEOUT=90s
PIPELINE_POLL_INTERVAL=3s
PIPELINE_REQUEST_TIMEOUT=20s
EOF
)"
}

create_webmail_unit() {
  local unit_file="${TMP_DIR}/${WEBMAIL_SERVICE_NAME}.service"

  cat > "${unit_file}" <<EOF
[Unit]
Description=go-register web_mail service
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=${TARGET_USER}
Environment=HOME=${TARGET_HOME}
Environment=USER=${TARGET_USER}
EnvironmentFile=-${WEBMAIL_ENV_FILE}
WorkingDirectory=${WORK_DIR}
ExecStart=${APP_DIR}/${BINARY_NAME} -mode webmail -web-mail-host \${WEBMAIL_HOST} -web-mail-port \${WEBMAIL_PORT} -web-mail-emails-file \${WEBMAIL_EMAILS_FILE} -mail-api-base \${MAIL_API_BASE} -web-mail-lease-timeout-seconds \${WEBMAIL_LEASE_TIMEOUT_SECONDS}
Restart=always
RestartSec=3

[Install]
WantedBy=multi-user.target
EOF

  run_as_root install -m 0644 "${unit_file}" "${SERVICE_DIR}/${WEBMAIL_SERVICE_NAME}.service"
}

create_pipeline_unit() {
  local unit_file="${TMP_DIR}/${PIPELINE_SERVICE_NAME}.service"

  cat > "${unit_file}" <<EOF
[Unit]
Description=go-register pipeline service
After=network-online.target ${WEBMAIL_SERVICE_NAME}.service
Wants=network-online.target ${WEBMAIL_SERVICE_NAME}.service

[Service]
Type=simple
User=${TARGET_USER}
Environment=HOME=${TARGET_HOME}
Environment=USER=${TARGET_USER}
EnvironmentFile=-${PIPELINE_ENV_FILE}
WorkingDirectory=${WORK_DIR}
ExecStart=${APP_DIR}/${BINARY_NAME} -mode pipeline -accounts-file \${PIPELINE_ACCOUNTS_FILE} -auth-dir \${PIPELINE_AUTH_DIR} -proxy \${PIPELINE_PROXY} -web-mail-url \${PIPELINE_WEBMAIL_URL} -count \${PIPELINE_COUNT} -workers \${PIPELINE_WORKERS} -authorize-workers \${PIPELINE_AUTHORIZE_WORKERS} -mailbox \${PIPELINE_MAILBOX} -timeout \${PIPELINE_TIMEOUT} -otp-timeout \${PIPELINE_OTP_TIMEOUT} -poll-interval \${PIPELINE_POLL_INTERVAL} -request-timeout \${PIPELINE_REQUEST_TIMEOUT}
Restart=on-failure
RestartSec=5

[Install]
WantedBy=multi-user.target
EOF

  run_as_root install -m 0644 "${unit_file}" "${SERVICE_DIR}/${PIPELINE_SERVICE_NAME}.service"
}

install_systemd_units() {
  create_webmail_unit
  create_pipeline_unit

  log "重载 systemd 并启动服务"
  run_as_root systemctl daemon-reload
  run_as_root systemctl enable --now "${WEBMAIL_SERVICE_NAME}.service"
  run_as_root systemctl enable --now "${PIPELINE_SERVICE_NAME}.service"
}

print_next_steps() {
  cat <<EOF

[install-system] 安装完成。
[install-system] 服务信息：
- ${WEBMAIL_SERVICE_NAME}.service：webmail 常驻服务
- ${PIPELINE_SERVICE_NAME}.service：pipeline 服务

[install-system] 默认安装目录：
- ${APP_DIR}

[install-system] 默认配置文件：
- ${WORK_DIR}/.config.json
- ${WEBMAIL_ENV_FILE}
- ${PIPELINE_ENV_FILE}

[install-system] 常用命令：
- 查看 webmail 状态：
  systemctl status ${WEBMAIL_SERVICE_NAME}.service
- 查看 pipeline 状态：
  systemctl status ${PIPELINE_SERVICE_NAME}.service
- 查看 webmail 日志：
  journalctl -u ${WEBMAIL_SERVICE_NAME}.service -f
- 查看 pipeline 日志：
  journalctl -u ${PIPELINE_SERVICE_NAME}.service -f

[install-system] 可选环境变量：
- GITHUB_REPO=${REPO}
- GO_REGISTER_TAG=${TAG}
- INSTALL_DIR=${APP_DIR}
- WORK_DIR=${WORK_DIR}
- WEBMAIL_ENV_FILE=${WEBMAIL_ENV_FILE}
- PIPELINE_ENV_FILE=${PIPELINE_ENV_FILE}

[install-system] 注意：
- 请先编辑 ${WORK_DIR}/emails.txt，填入邮箱池数据
- 服务参数默认从 ${WEBMAIL_ENV_FILE} 和 ${PIPELINE_ENV_FILE} 读取；修改后请执行 systemctl restart
- ${PIPELINE_SERVICE_NAME}.service 使用 pipeline 模式；达到 PIPELINE_COUNT=${PIPELINE_COUNT} 后会正常退出
EOF
}

main() {
  log "开始安装 ${BINARY_NAME} systemd 服务"
  resolve_target_user
  resolve_platform
  resolve_download_url
  download_asset
  extract_binary
  install_binary
  prepare_runtime_files
  install_systemd_units
  print_next_steps
}

main "$@"
