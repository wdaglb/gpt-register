#!/usr/bin/env bash

set -euo pipefail

REPO="${GITHUB_REPO:-wdaglb/gpt-register}"
BINARY_NAME="${GO_REGISTER_BINARY_NAME:-go-register}"
TAG="${GO_REGISTER_TAG:-latest}"
# Why: 安装脚本默认把二进制和运行态文件都收口到同一个目录，
# 避免用户还要额外区分 PATH 下的二进制目录和项目运行目录。
DEFAULT_APP_DIR="${HOME}/gpt-register"
INSTALL_DIR="${INSTALL_DIR:-${DEFAULT_APP_DIR}}"
WORK_DIR="${WORK_DIR:-${DEFAULT_APP_DIR}}"
TMP_DIR="$(mktemp -d)"

log() {
  printf '[install] %s\n' "$*"
}

warn() {
  printf '[install][warn] %s\n' "$*" >&2
}

fail() {
  printf '[install][error] %s\n' "$*" >&2
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

resolve_platform() {
  local os_name
  local arch_name

  os_name="$(uname -s)"
  arch_name="$(uname -m)"

  case "${os_name}" in
    Linux) GOOS="linux" ;;
    Darwin) GOOS="darwin" ;;
    *)
      fail "当前安装脚本仅支持 Linux / macOS，检测到系统: ${os_name}"
      ;;
  esac

  case "${arch_name}" in
    x86_64|amd64) GOARCH="amd64" ;;
    arm64|aarch64) GOARCH="arm64" ;;
    *)
      fail "当前安装脚本暂不支持该架构: ${arch_name}"
      ;;
  esac

  ARCHIVE_EXT="tar.gz"
  ASSET_NAME="${BINARY_NAME}_${GOOS}_${GOARCH}.${ARCHIVE_EXT}"
}

resolve_download_url() {
  # Why: 线上默认走 latest release，便于主分支每次自动发布后都能被安装脚本直接消费；
  # 若用户指定具体 tag，则回退到固定版本下载链接。
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
  mkdir -p "${INSTALL_DIR}"
  install -m 0755 "${EXTRACTED_BINARY}" "${INSTALL_DIR}/${BINARY_NAME}"
  log "二进制已安装到: ${INSTALL_DIR}/${BINARY_NAME}"

  if [[ ":${PATH}:" != *":${INSTALL_DIR}:"* ]]; then
    warn "INSTALL_DIR 未加入 PATH，当前会话请使用完整路径运行"
  fi
}

prepare_runtime_files() {
  # Why: 二进制本身不带运行态数据；这里初始化最小目录结构，用户解压后即可直接按 README 命令运行。
  mkdir -p "${WORK_DIR}/auth"
  touch "${WORK_DIR}/accounts.txt"
  touch "${WORK_DIR}/emails.txt"
  touch "${WORK_DIR}/user.txt"
  write_default_config
  log "已初始化运行目录: ${WORK_DIR}"
}

write_default_config() {
  local config_path="${WORK_DIR}/.config.json"

  # Why: 安装脚本直接生成一份可运行的默认配置，用户首次进入 TUI 时可以立刻看到完整参数，
  # 同时通过“已存在则跳过”避免重装时覆盖用户手工修改过的配置。
  write_file_if_missing "${config_path}" "$(cat <<EOF
{
  "mode": "pipeline",
  "web-mail-url": "http://127.0.0.1:8030",
  "email": "",
  "password": "",
  "user-file": "user.txt",
  "auth-dir": "auth",
  "accounts-file": "accounts.txt",
  "proxy": "http://127.0.0.1:7890",
  "mailbox": "Junk",
  "count": 5,
  "workers": 2,
  "authorize-workers": 2,
  "timeout": "4m0s",
  "otp-timeout": "1m30s",
  "poll-interval": "3s",
  "request-timeout": "20s"
}
EOF
)"
}

print_next_steps() {
  cat <<EOF

[install] 安装完成。
[install] 下一步建议：
1. 进入安装目录：
   cd ${WORK_DIR}
2. 编辑 ./emails.txt，填入邮箱池数据，格式：
   email@example.com----password----client_id----refresh_token
3. 默认配置文件：
   ./.config.json
4. 启动内置 web_mail：
   ./${BINARY_NAME} -mode webmail -web-mail-host 127.0.0.1 -web-mail-port 8030 -web-mail-emails-file ./emails.txt
5. 启动主程序（TUI 推荐）：
   ./${BINARY_NAME} -accounts-file ./accounts.txt -proxy http://127.0.0.1:7890 -web-mail-url http://127.0.0.1:8030

[install] 可选环境变量：
- GITHUB_REPO=${REPO}
- GO_REGISTER_TAG=${TAG}
- INSTALL_DIR=${INSTALL_DIR}
- WORK_DIR=${WORK_DIR}
EOF
}

main() {
  log "开始安装 ${BINARY_NAME}"
  resolve_platform
  resolve_download_url
  download_asset
  extract_binary
  install_binary
  prepare_runtime_files
  print_next_steps
}

main "$@"
