#!/usr/bin/env bash
# install.sh — agent-relay 一键安装/升级脚本（跑在 VPS 上，需 root 或 sudo）。
#
# 用法：
#   curl -sSL https://raw.githubusercontent.com/AlixWang/agent-relay/main/deploy/install.sh | sudo bash
#   curl -sSL .../install.sh | sudo bash -s -- --version v0.1.2 --mode docker
#
# 交互式向导：版本 → 部署方式（systemd 二进制 / docker）→
# 网络模式（tailscale 内网 / 公网 TLS / 仅本机）→ admin 密码 → 端口/目录。
# 非交互：全部可用 --flag 预设（--help 查看），CI/无人值守可用。
set -euo pipefail

REPO="AlixWang/agent-relay"
IMAGE="ghcr.io/alixwang/agent-relay"
BIN_PATH="/usr/local/bin/agent-relay"
CONFIG_PATH="/etc/agent-relay/config.toml"
SERVICE_PATH="/etc/systemd/system/agent-relay.service"
DATA_DIR_DEFAULT="/var/lib/agent-relay"
PORT_DEFAULT=18789

VERSION=""            # 空 = 最新 Release
MODE=""               # systemd | docker
NET=""                # tailscale | public | loopback
ADMIN_PW=""           # 空 = 交互输入（两次确认），--random-pw 则随机生成
PORT="$PORT_DEFAULT"
DATA_DIR="$DATA_DIR_DEFAULT"
TLS_CERT=""
TLS_KEY=""
YES=0

log()  { printf '\033[1;32m[install]\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m[warn]\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[1;31m[error]\033[0m %s\n' "$*" >&2; exit 1; }

usage() {
  # NOTE: quoted heredoc (no expansion) — macOS bash 3.2 mangles $VAR
  # after multibyte chars inside heredocs; defaults printed separately.
  cat <<'EOF'
用法: install.sh [选项]
  --version TAG      指定版本（如 v0.1.2），默认最新 Release
  --mode M           systemd | docker（默认交互选择）
  --net N            tailscale | public | loopback（默认交互选择）
  --admin-pw PW      admin 密码（默认交互输入；生产请用强密码）
  --random-pw        随机生成 admin 密码并打印（无人值守用）
  --port P           监听端口
  --data-dir D       数据目录
  --tls-cert F       公网模式证书（--net public 必填）
  --tls-key F        公网模式私钥（--net public 必填）
  -y, --yes          全自动（未给的选项用默认值：systemd+tailscale）
  -h, --help         显示本帮助
EOF
  printf '默认端口: %s, 数据目录: %s\n' "$PORT_DEFAULT" "$DATA_DIR_DEFAULT"
}

while [ $# -gt 0 ]; do
  case "$1" in
    --version) VERSION="$2"; shift 2;;
    --mode) MODE="$2"; shift 2;;
    --net) NET="$2"; shift 2;;
    --admin-pw) ADMIN_PW="$2"; shift 2;;
    --random-pw) ADMIN_PW="__RANDOM__"; shift;;
    --port) PORT="$2"; shift 2;;
    --data-dir) DATA_DIR="$2"; shift 2;;
    --tls-cert) TLS_CERT="$2"; shift 2;;
    --tls-key) TLS_KEY="$2"; shift 2;;
    -y|--yes) YES=1; shift;;
    -h|--help) usage; exit 0;;
    *) die "未知选项 $1（--help 查看）";;
  esac
done

[ "$(id -u)" -eq 0 ] || die "请用 root 或 sudo 运行"
command -v curl >/dev/null || die "需要 curl"
command -v jq >/dev/null || warn "建议安装 jq（版本解析更稳），将用 grep 兜底"

ARCH_RAW="$(uname -m)"
case "$ARCH_RAW" in
  x86_64) ARCH=amd64;;
  aarch64|arm64) ARCH=arm64;;
  *) die "不支持的架构 $ARCH_RAW（仅 amd64/arm64）";;
esac

ask() { # $1 提示 $2 默认值 -> 输出到 stdout
  local prompt="$1" def="$2" ans
  if [ "$YES" -eq 1 ]; then printf '%s' "$def"; return; fi
  if [ -n "$def" ]; then printf '%s [%s]: ' "$prompt" "$def" >&2; else printf '%s: ' "$prompt" >&2; fi
  read -r ans || true
  printf '%s' "${ans:-$def}"
}

ask_secret() { # $1 提示 -> stdout（不回显）
  local prompt="$1" a b
  if [ "$YES" -eq 1 ]; then die "$prompt 未提供（-y 模式请加 --admin-pw 或 --random-pw）"; fi
  printf '%s: ' "$prompt" >&2; stty -echo; read -r a; stty echo; printf '\n' >&2
  printf '%s（确认）: ' "$prompt" >&2; stty -echo; read -r b; stty echo; printf '\n' >&2
  [ "$a" = "$b" ] || die "两次输入不一致"
  [ -n "$a" ] || die "密码不能为空"
  printf '%s' "$a"
}

latest_tag() {
  local tag
  tag=$(curl -sSL "https://api.github.com/repos/$REPO/releases/latest" | { jq -r .tag_name 2>/dev/null || grep -o '"tag_name": *"[^"]*"' | head -1 | cut -d'"' -f4; })
  [ -n "$tag" ] && [ "$tag" != "null" ] || die "取最新版本失败（网络或 API 限流），可用 --version 指定"
  printf '%s' "$tag"
}

# ---- 1. 版本 ----
if [ -z "$VERSION" ]; then
  if [ "$YES" -eq 1 ]; then VERSION="$(latest_tag)"; log "使用最新版本 $VERSION"
  else
    LATEST="$(latest_tag)"
    VERSION="$(ask "版本（回车=最新 $LATEST）" "$LATEST")"
  fi
fi
log "版本: $VERSION  架构: $ARCH"

# ---- 2. 部署方式 ----
if [ -z "$MODE" ]; then
  echo "部署方式：1) systemd 二进制（推荐，单文件+SQLite）  2) docker（GHCR 镜像）" >&2
  case "$(ask "选 [1/2]" "1")" in
    2|docker) MODE=docker;; *) MODE=systemd;;
  esac
fi
case "$MODE" in systemd|docker) ;; *) die "--mode 只能是 systemd|docker";; esac

# ---- 3. 网络模式 ----
if [ -z "$NET" ]; then
  echo "网络模式：1) tailscale 内网（推荐，只绑 tailnet IP）  2) 公网 TLS（需证书）  3) 仅本机 127.0.0.1" >&2
  case "$(ask "选 [1/2/3]" "1")" in
    2|public) NET=public;; 3|loopback) NET=loopback;; *) NET=tailscale;;
  esac
fi
LISTEN=""
case "$NET" in
  tailscale)
    if command -v tailscale >/dev/null && TSIP="$(tailscale ip -4 2>/dev/null | head -1)" && [ -n "$TSIP" ]; then
      LISTEN="$TSIP"; log "检测到 tailnet IP: $TSIP"
    else
      warn "本机无 tailscale 或未上线，listen_addr 用 auto（启动时自动探测，失败回退 127.0.0.1）"
      LISTEN="auto"
    fi
    ;;
  public)
    [ -n "$TLS_CERT" ] && [ -n "$TLS_KEY" ] || {
      [ "$YES" -eq 1 ] && die "公网模式需 --tls-cert/--tls-key"
      TLS_CERT="$(ask "TLS 证书路径（fullchain.pem）" "")"
      TLS_KEY="$(ask "TLS 私钥路径（privkey.pem）" "")"
      [ -n "$TLS_CERT" ] && [ -n "$TLS_KEY" ] || die "公网模式必须提供证书"
    }
    [ -f "$TLS_CERT" ] && [ -f "$TLS_KEY" ] || die "证书文件不存在"
    LISTEN="0.0.0.0"
    ;;
  loopback) LISTEN="127.0.0.1";;
  *) die "--net 只能是 tailscale|public|loopback";;
esac
if [ -z "$LISTEN" ]; then LISTEN="auto"; fi
# 允许用户微调
if [ "$YES" -eq 0 ]; then LISTEN="$(ask "监听地址" "$LISTEN")"; PORT="$(ask "端口" "$PORT")"; DATA_DIR="$(ask "数据目录" "$DATA_DIR")"; fi

# ---- 4. admin 密码 ----
GEN_PW=0
if [ "$ADMIN_PW" = "__RANDOM__" ]; then
  ADMIN_PW="$(tr -dc 'A-Za-z0-9' </dev/urandom | head -c 24)"; GEN_PW=1
elif [ -z "$ADMIN_PW" ]; then
  ADMIN_PW="$(ask_secret "设置 admin 密码")"
fi

# ---- 5. 安装二进制 / 镜像 ----
install_binary() {
  local url="https://github.com/$REPO/releases/download/$VERSION/agent-relay-linux-$ARCH"
  local sums="https://github.com/$REPO/releases/download/$VERSION/SHA256SUMS"
  log "下载 $url"
  curl -fSL -o /tmp/agent-relay "$url" || die "下载失败（检查版本 $VERSION 是否存在、架构是否匹配）"
  log "校验 SHA256…"
  EXPECTED="$(curl -fSL "$sums" | grep "agent-relay-linux-$ARCH" | awk '{print $1}')" \
    || die "SHA256SUMS 下载失败"
  ACTUAL="$(sha256sum /tmp/agent-relay | awk '{print $1}')"
  [ "$EXPECTED" = "$ACTUAL" ] || die "校验和不匹配！期望 $EXPECTED 实际 $ACTUAL（可能被劫持或下载损坏）"
  log "校验通过"
  install -m 755 /tmp/agent-relay "$BIN_PATH"
  rm -f /tmp/agent-relay
  "$BIN_PATH" -version || true
}

if [ "$MODE" = "docker" ]; then
  command -v docker >/dev/null || die "需要 docker（--mode systemd 则不需要）"
  log "拉取 $IMAGE:$VERSION"
  docker pull "$IMAGE:$VERSION" || die "拉镜像失败（私有仓库先 docker login ghcr.io）"
else
  install_binary
fi

# ---- 6. 写配置 ----
HASH=""
if [ "$MODE" = "docker" ]; then
  # 容器内同样用二进制生成 hash：一次性运行，不挂载数据卷
  HASH="$(docker run --rm --entrypoint /usr/local/bin/agent-relay "$IMAGE:$VERSION" -hash "$ADMIN_PW")" \
    || die "生成密码 hash 失败"
else
  HASH="$("$BIN_PATH" -hash "$ADMIN_PW")" || die "生成密码 hash 失败"
fi

mkdir -p "$(dirname "$CONFIG_PATH")" "$DATA_DIR"
if [ -f "$CONFIG_PATH" ]; then
  cp "$CONFIG_PATH" "$CONFIG_PATH.bak.$(date +%Y%m%d%H%M%S)"
  log "已备份旧配置"
fi

if [ "$NET" = "public" ]; then PUBLIC=true; else PUBLIC=false; fi
cat > "$CONFIG_PATH" <<EOF
# Generated by deploy/install.sh. version $VERSION. See docs/CONFIG.md.
listen_addr = "$LISTEN"
port = $PORT
data_dir = "/var/lib/agent-relay"
public = $PUBLIC
tls_cert = "$TLS_CERT"
tls_key = "$TLS_KEY"
admin_password_hash = "$HASH"
protocol = 1
min_client = 1
online_timeout_secs = 300
verify_timeout_secs = 600
fuse_max_messages = 50
fuse_max_age_secs = 86400
rate_per_minute = 60
message_ttl_days = 30
peer_prune_after_days = 7
audit_retention_days = 90
offline_webhook_url = ""
max_body_bytes = 1048576
EOF
chmod 600 "$CONFIG_PATH"
# docker 模式数据目录归属：镜像内 agent-relay 用户 uid 未定，运行时用 --user 或放宽目录权限
if [ "$MODE" = "docker" ]; then chmod 777 "$DATA_DIR" 2>/dev/null || true; fi
log "配置已写入 $CONFIG_PATH"

# docker 模式要把宿主 data_dir 映射到容器内固定路径，config 里 data_dir 需同步
if [ "$MODE" = "docker" ]; then
  # 容器内约定 /var/lib/agent-relay；宿主目录通过 -v 挂载
  sed -i 's|^data_dir = .*|data_dir = "/var/lib/agent-relay"|' "$CONFIG_PATH"
  if [ "$NET" = "public" ]; then
    # 证书挂载到容器内固定路径，config 必须同步改写
    sed -i 's|^tls_cert = .*|tls_cert = "/certs/fullchain.pem"|' "$CONFIG_PATH"
    sed -i 's|^tls_key = .*|tls_key = "/certs/privkey.pem"|' "$CONFIG_PATH"
  fi
fi

# ---- 7. 启动 ----
if [ "$MODE" = "docker" ]; then
  CNAME="agent-relay"
  docker rm -f "$CNAME" >/dev/null 2>&1 || true
  if [ "$NET" = "public" ]; then
    # 证书挂只读进容器；若用宿主固定路径则直接挂
    docker run -d --name "$CNAME" --restart unless-stopped \
      -v "$DATA_DIR:/var/lib/agent-relay" \
      -v "$TLS_CERT:/certs/fullchain.pem:ro" \
      -v "$TLS_KEY:/certs/privkey.pem:ro" \
      -v "$CONFIG_PATH:/etc/agent-relay/config.toml:ro" \
      -p "$PORT:$PORT" \
      "$IMAGE:$VERSION" >/dev/null
  else
    if [ "$NET" = "tailscale" ]; then
      # 容器需共享宿主网络才能拿到 tailnet IP
      docker run -d --name "$CNAME" --restart unless-stopped --network host \
        -v "$DATA_DIR:/var/lib/agent-relay" \
        -v "$CONFIG_PATH:/etc/agent-relay/config.toml:ro" \
        "$IMAGE:$VERSION" >/dev/null
    else
      docker run -d --name "$CNAME" --restart unless-stopped \
        -v "$DATA_DIR:/var/lib/agent-relay" \
        -v "$CONFIG_PATH:/etc/agent-relay/config.toml:ro" \
        -p "127.0.0.1:$PORT:$PORT" \
        "$IMAGE:$VERSION" >/dev/null
    fi
  fi
  sleep 2
  docker logs --tail 5 "$CNAME" || true
else
  # systemd：service 文件内嵌（与 deploy/agent-relay.service 同步，支持 --port 覆盖）
  cat > "$SERVICE_PATH" <<EOF
[Unit]
Description=agent-relay — AI assistant message relay
Documentation=https://github.com/$REPO
After=network-online.target tailscaled.service
Wants=network-online.target

[Service]
Type=simple
User=agent-relay
Group=agent-relay
ExecStartPre=/bin/mkdir -p $DATA_DIR
ExecStartPre=/bin/chown agent-relay:agent-relay $DATA_DIR
ExecStart=$BIN_PATH -config $CONFIG_PATH
Restart=always
RestartSec=5
# SQLite on local disk only; hardening:
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=$DATA_DIR
NoNewPrivileges=true
PrivateTmp=true

[Install]
WantedBy=multi-user.target
EOF
  id agent-relay >/dev/null 2>&1 || useradd -r -d "$DATA_DIR" -s /usr/sbin/nologin agent-relay
  mkdir -p "$DATA_DIR" && chown agent-relay:agent-relay "$DATA_DIR"
  # 配置里的 data_dir 与 service 的 ReadWritePaths 保持一致
  sed -i "s|^data_dir = .*|data_dir = \"$DATA_DIR\"|" "$CONFIG_PATH"
  systemctl daemon-reload
  systemctl enable --now agent-relay
  sleep 2
  systemctl --no-pager status agent-relay | head -8 || true
fi

# ---- 8. 健康检查 ----
sleep 1
if [ "$LISTEN" = "auto" ]; then CHECK_HOST="127.0.0.1"; elif [ "$LISTEN" = "0.0.0.0" ]; then CHECK_HOST="127.0.0.1"; else CHECK_HOST="$LISTEN"; fi
SCHEME="http"; [ "$NET" = "public" ] && SCHEME="https"
if curl -sk --max-time 5 "$SCHEME://$CHECK_HOST:$PORT/health" | grep -q '"ok":true'; then
  log "健康检查通过：$SCHEME://$CHECK_HOST:$PORT/health"
else
  warn "健康检查未通过，查看日志："
  if [ "$MODE" = "docker" ]; then docker logs --tail 20 agent-relay || true
  else journalctl -u agent-relay --no-pager -n 20 || true; fi
  die "启动失败，按上面日志排查（常见：端口占用 / tailscale 未上线 / 证书路径错）"
fi

echo
log "部署完成！"
echo "  Web 控制台： $SCHEME://$CHECK_HOST:$PORT/ （用刚才设的 admin 密码登录）"
[ "$GEN_PW" -eq 1 ] && echo "  随机 admin 密码： $ADMIN_PW  ← 只显示这一次，请收好"
echo "  下一步：进 Web UI → 邀请 & Prompt → 生成首个助手邀请（见 docs/ONBOARDING.md）"
echo "  升级：重跑本脚本并 --version 新版本（数据目录不动，schema 自动迁移）"
