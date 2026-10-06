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
NET=""                # tailscale | public | loopback | caddy
PROXY_DOMAIN=""      # --net caddy 必填：公网域名（如 relay.example.com）
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
  --net N            tailscale | public | loopback | caddy（默认交互选择）
  --proxy-domain D   反代公网域名（--net caddy 必填）
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
    --proxy-domain) PROXY_DOMAIN="$2"; shift 2;;
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

# NOTE: 所有交互输入一律走 /dev/tty。curl | bash 时脚本的 stdin 是下载管道
# （已 EOF），从 stdin read 永远读到空、stty 直接报错。--yes 模式跳过交互。
need_tty() {
  # Actually try opening /dev/tty: [ -c ] can lie under sudo/no-tty.
  [ "$YES" -eq 0 ] && exec 9<>/dev/tty 2>/dev/null || return 1
  exec 9>&- 2>/dev/null || true
  return 0
}

ask() { # $1 提示 $2 默认值 -> 输出到 stdout
  local prompt="$1" def="$2" ans
  if [ "$YES" -eq 1 ]; then printf '%s' "$def"; return; fi
  need_tty || die "无交互终端：请加 -y 用默认值，或用 --flag 预设（--help 查看）"
  if [ -n "$def" ]; then printf '%s [%s]: ' "$prompt" "$def" >&2; else printf '%s: ' "$prompt" >&2; fi
  read -r ans </dev/tty || true
  printf '%s' "${ans:-$def}"
}

ask_secret() { # $1 提示 -> stdout（不回显）
  local prompt="$1" a b
  if [ "$YES" -eq 1 ]; then die "$prompt 未提供（-y 模式请加 --admin-pw 或 --random-pw）"; fi
  need_tty || die "无交互终端：请用 --admin-pw 或 --random-pw 提供密码"
  printf '%s: ' "$prompt" >&2; stty -echo </dev/tty; read -r a </dev/tty; stty echo </dev/tty; printf '\n' >&2
  printf '确认: ' >&2; stty -echo </dev/tty; read -r b </dev/tty; stty echo </dev/tty; printf '\n' >&2
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

# systemd_unit $name — 进程名/服务名 -> 完整 unit 名（caddy -> caddy.service）。
# 用 systemctl show 探测，不依赖 list-units 文本格式。
systemd_unit() {
  local name="$1" u
  for u in "$name" "$name.service"; do
    if systemctl show "$u" --property=LoadState 2>/dev/null | grep -q "=loaded"; then
      printf '%s' "$u"
      return 0
    fi
  done
  return 1
}

is_systemd_service() {
  [ -n "$(systemd_unit "$1" || true)" ]
}

# issue_cert $domain — certbot standalone 申请 + 自动续期（需 root）。
# 要求：域名已解析到本机公网 IP，且 80 端口空闲（standalone 会临时占用）。
issue_cert() {
  local domain="$1" pub_ip resolved
  command -v certbot >/dev/null || {
    log "安装 certbot…"
    apt-get update -qq && apt-get install -y -qq certbot || die "certbot 安装失败"
  }
  pub_ip="$(curl -s --max-time 10 https://api.ipify.org || true)"
  resolved="$(getent hosts "$domain" | awk '{print $1}' | head -1 || true)"
  log "本机公网 IP: ${pub_ip:-未知}，$domain 解析到: ${resolved:-未知}"
  if [ -n "$pub_ip" ] && [ -n "$resolved" ] && [ "$pub_ip" != "$resolved" ]; then
    warn "$domain 未解析到本机（$resolved vs $pub_ip）"
    warn "若域名走了 CDN 代理（如 Cloudflare 小云朵），standalone 验证会失败："
    warn "  1) 去 DNS 把 $domain 切到『仅 DNS』（灰云），等生效；或"
    warn "  2) 用 DNS 验证：certbot certonly --manual --preferred-challenges dns -d $domain"
    die "域名未指向本机，停止申请（避免 LE 限流）"
  fi
  # 80 端口占用处理：standalone 要求 LE 能回连 http://域名/.well-known。
  # 占用者若是 systemd 服务，临时停掉，申请完再起回来。
  if ! command -v ss >/dev/null 2>&1 && command -v apt-get >/dev/null 2>&1; then
    log "安装 iproute2（检测 80 端口占用）…"
    apt-get install -y -qq iproute2 2>/dev/null || true
  fi
  local holder=""
  if command -v ss >/dev/null 2>&1; then
    holder="$(ss -ltnp 2>/dev/null | grep ':80 ' | grep -o 'users:(("[^"]*"' | head -1 | cut -d'"' -f2 || true)"
  fi
  if [ -z "$holder" ] && (echo >/dev/tcp/127.0.0.1/80) 2>/dev/null; then
    warn "80 端口被占用但查不到进程名"
    die "先手动停掉占 80 的服务再重跑；或改用 webroot/DNS 验证（见 docs/DEPLOY.md）"
  fi
  local stopped=""
  if [ -n "$holder" ]; then
    warn "80 端口被 $holder 占用"
    if is_systemd_service "$holder"; then
      unit="$(systemd_unit "$holder")"
      log "临时停止 $unit（证书申请完自动起回）…"
      systemctl stop "$unit" && stopped="$unit" || warn "停 $unit 失败，尝试继续（可能仍会申请失败）"
    else
      die "80 端口被非 systemd 进程（$holder）占用：先停掉它再重跑；或改用 webroot/DNS 验证（见 docs/DEPLOY.md §公网 TLS 手动模式）"
    fi
  fi
  log "申请证书：$domain（standalone，需 80 端口空闲）…"
  if certbot certonly --standalone --non-interactive --agree-tos \
    --register-unsafely-without-email -d "$domain"; then
    log "证书申请成功"
  else
    [ -n "$stopped" ] && systemctl start "$stopped" || true
    die "证书申请失败（排查：80 端口是否真空闲 / 域名解析是否刚改还没生效 / 防火墙是否放行 80）"
  fi
  if [ -n "$stopped" ]; then
    log "恢复 $stopped…"
    systemctl start "$stopped" || warn "$stopped 恢复失败，请手动 systemctl start $stopped"
  fi
  # 自动续期：独立包装脚本（每天 cron 调用）：让出 80 → renew → 恢复占位
  # 服务 → reload relay。certbot 自带 systemd timer 一般也已启用，双保险。
  cat > /usr/local/sbin/agent-relay-renew.sh <<'RENEW_EOF'
#!/usr/bin/env bash
# agent-relay certbot auto-renew wrapper (installed by deploy/install.sh).
set -uo pipefail
SVC=""
if command -v ss >/dev/null 2>&1; then
  SVC="$(ss -ltnp 2>/dev/null | grep ':80 ' | grep -o 'users:(("[^"]*"' | head -1 | cut -d'"' -f2 || true)"
fi
UNIT=""
if [ -n "$SVC" ]; then
  for u in "$SVC" "$SVC.service"; do
    if systemctl show "$u" --property=LoadState 2>/dev/null | grep -q "=loaded"; then
      UNIT="$u"
      break
    fi
  done
fi
if [ -n "$UNIT" ]; then
  systemctl stop "$UNIT" || UNIT=""
fi
certbot renew --quiet
RC=$?
if [ -n "$UNIT" ]; then systemctl start "$UNIT" || true; fi
if systemctl cat agent-relay >/dev/null 2>&1; then
  systemctl reload-or-restart agent-relay || true
elif command -v docker >/dev/null 2>&1 && docker inspect agent-relay >/dev/null 2>&1; then
  docker restart agent-relay || true
fi
exit "$RC"
RENEW_EOF
  chmod 755 /usr/local/sbin/agent-relay-renew.sh
  (crontab -l 2>/dev/null | grep -v "# agent-relay" || true
   echo "17 3 * * * /usr/local/sbin/agent-relay-renew.sh # agent-relay") | crontab - \
    && log "已加 cron 自动续期（每天 3:17，续期时自动让出 80 端口并 reload 服务）" || warn "cron 写入失败，请手动配续期"
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
  echo "网络模式：" >&2
  echo "  1) tailscale 内网（推荐，只绑 tailnet IP）" >&2
  echo "  2) 公网 TLS（relay 自己持证书，需 80 空闲或已有证书）" >&2
  echo "  3) 仅本机 127.0.0.1" >&2
  echo "  4) caddy 反代共存（80/443 已被 caddy 占用，TLS 由 caddy 终止）" >&2
  case "$(ask "选 [1/2/3/4]" "1")" in
    2|public) NET=public;; 3|loopback) NET=loopback;; 4|caddy) NET=caddy;; *) NET=tailscale;;
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
    if [ -z "$TLS_CERT" ] || [ -z "$TLS_KEY" ]; then
      [ "$YES" -eq 1 ] && die "公网模式需 --tls-cert/--tls-key（或交互运行走申请向导）"
      echo "证书：1) 我已有证书文件，手动填路径  2) 用域名自动申请（certbot，需 80 端口空闲）  3) 回到 tailscale 内网模式" >&2
      CERT_CHOICE="$(ask "选 [1/2/3]" "2")"
      case "$CERT_CHOICE" in
        1)
          TLS_CERT="$(ask "TLS 证书路径（fullchain.pem）" "")"
          TLS_KEY="$(ask "TLS 私钥路径（privkey.pem）" "")"
          [ -n "$TLS_CERT" ] && [ -n "$TLS_KEY" ] || die "证书路径不能为空"
          ;;
        3) NET=tailscale; LISTEN="auto"
          log "已切回 tailscale 模式"
          ;;
        *)
          DOMAIN="$(ask "域名（如 relay.example.com）" "")"
          [ -n "$DOMAIN" ] || die "域名不能为空"
          issue_cert "$DOMAIN"
          TLS_CERT="/etc/letsencrypt/live/$DOMAIN/fullchain.pem"
          TLS_KEY="/etc/letsencrypt/live/$DOMAIN/privkey.pem"
          ;;
      esac
    fi
    if [ "$NET" = "public" ]; then
      [ -f "$TLS_CERT" ] && [ -f "$TLS_KEY" ] || die "证书文件不存在：$TLS_CERT / $TLS_KEY"
      LISTEN="0.0.0.0"
    fi
    ;;
  loopback) LISTEN="127.0.0.1";;
  caddy)
    # 反代共存：relay 只听本机明文，TLS 由 caddy 终止。
    # 域名必须已解析到本机（caddy 自动 HTTPS 靠它拿证）。
    if [ -z "$PROXY_DOMAIN" ]; then
      [ "$YES" -eq 1 ] && die "caddy 模式需 --proxy-domain"
      PROXY_DOMAIN="$(ask "反代公网域名（如 relay.example.com，须已解析到本机）" "")"
      [ -n "$PROXY_DOMAIN" ] || die "域名不能为空"
    fi
    command -v caddy >/dev/null || die "未检测到 caddy（$PROXY_DOMAIN 的 80/443 应由它接管）。先装 caddy 或改选其他网络模式"
    LISTEN="127.0.0.1"
    ;;
  *) die "--net 只能是 tailscale|public|loopback|caddy";;
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
if [ "$NET" = "caddy" ]; then BEHIND_PROXY=true; PUBLIC_ADDR="https://$PROXY_DOMAIN"; else BEHIND_PROXY=false; PUBLIC_ADDR=""; fi
cat > "$CONFIG_PATH" <<EOF
# Generated by deploy/install.sh. version $VERSION. See docs/CONFIG.md.
listen_addr = "$LISTEN"
port = $PORT
data_dir = "/var/lib/agent-relay"
public = $PUBLIC
tls_cert = "$TLS_CERT"
tls_key = "$TLS_KEY"
behind_proxy = $BEHIND_PROXY
public_addr = "$PUBLIC_ADDR"
trusted_proxy = "127.0.0.1/32"
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
stream_keepalive_secs = 20
stream_max_per_peer = 3
profile_refresh_days = 7
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

# ---- 6.5 caddy 反代站点（仅 --net caddy） ----
if [ "$NET" = "caddy" ]; then
  SITE_FILE="/etc/caddy/sites/agent-relay.conf"
  if [ -f "$SITE_FILE" ]; then
    cp "$SITE_FILE" "$SITE_FILE.bak.$(date +%Y%m%d%H%M%S)"
    log "已备份旧 caddy 站点"
  fi
  cat > "$SITE_FILE" <<EOF
# agent-relay reverse proxy (installed by deploy/install.sh $VERSION).
# TLS terminates at caddy (automatic HTTPS); relay listens on loopback only.
$PROXY_DOMAIN {
	reverse_proxy 127.0.0.1:$PORT
}
EOF
  log "caddy 站点已写入 $SITE_FILE"
  if caddy validate --config /etc/caddy/Caddyfile --adapter caddyfile 2>/dev/null; then
    log "caddy 配置校验通过"
  else
    warn "caddy 配置校验失败（可能是 import 路径不同），请手动检查 $SITE_FILE 是否被主 Caddyfile import"
  fi
  # NOTE: admin API 被禁（admin off）的 caddy 上 reload 必失败，用 restart。
  if caddy reload 2>/dev/null; then
    log "caddy 已 reload（自动申请 $PROXY_DOMAIN 证书）"
  else
    warn "caddy reload 失败（常见于 admin off 配置），改用 restart…"
    systemctl restart caddy && log "caddy 已 restart" || die "caddy 启动失败：systemctl status caddy 看日志"
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
  mkdir -p "$DATA_DIR" && chown -R agent-relay:agent-relay "$DATA_DIR"
  # config 含 bcrypt hash：属主给服务用户（600），否则 ProtectSystem 下读不到
  chown agent-relay:agent-relay "$CONFIG_PATH" && chmod 600 "$CONFIG_PATH"
  # 配置里的 data_dir 与 service 的 ReadWritePaths 保持一致
  sed -i "s|^data_dir = .*|data_dir = \"$DATA_DIR\"|" "$CONFIG_PATH"
  systemctl daemon-reload
  systemctl enable --now agent-relay
  sleep 2
  systemctl --no-pager status agent-relay | head -8 || true
fi

# ---- 7.5 Web 一键更新 helper（仅 systemd 模式） ----
# Web 控制台 `POST /admin/update/apply` 以 agent-relay 用户经 sudo 调它：
# 下载→SHA256→备份→安装→重启→健康检查→失败回滚。root 属主 0700 + sudoers
# 仅放行 `agent-relay-update apply <v*>`，服务本身不直接提权。
if [ "$MODE" = "systemd" ]; then
  HELPER="/usr/local/sbin/agent-relay-update"
  cat > "$HELPER" <<HELPER_EOF
#!/usr/bin/env bash
# agent-relay-update — Web 一键更新 helper (installed by deploy/install.sh).
# Usage: agent-relay-update apply <version>  (version must match ^v[0-9])
# Logs to stderr, machine-readable tail line: UPDATE_RESULT <ok|rolled_back|failed> <detail>
set -uo pipefail
REPO="$REPO"
BIN_PATH="$BIN_PATH"
HELPER_EOF
  cat >> "$HELPER" <<EOF
JOB_DIR="$DATA_DIR/update-jobs"
logf() { printf '[update] %s\n' "\$*" >&2; }
result() { printf 'UPDATE_RESULT %s %s\n' "\$1" "\$2"; }

cmd="\${1:-}" ver="\${2:-}"
[ "\$cmd" = "apply" ] || { result failed "usage: apply <version>"; exit 2; }
case "\$ver" in v[0-9]*) ;; *) result failed "bad version"; exit 2;; esac
case "\$ver" in *[^A-Za-z0-9._-]* ) result failed "bad version chars"; exit 2;; esac

ARCH_RAW="\$(uname -m)"
case "\$ARCH_RAW" in x86_64) ARCH=amd64;; aarch64|arm64) ARCH=arm64;; *) result failed "unsupported arch"; exit 2;; esac

mkdir -p "\$JOB_DIR"
JOB="\$JOB_DIR/\$ver.log"
: > "\$JOB"
{
logf "target \$ver arch \$ARCH"
TMP="\$(mktemp -d)"
URL="https://github.com/\$REPO/releases/download/\$ver/agent-relay-linux-\$ARCH"
SUMS="https://github.com/\$REPO/releases/download/\$ver/SHA256SUMS"
curl -fSL -o "\$TMP/agent-relay" "\$URL" || { logf "download failed"; result failed "download"; exit 1; }
EXPECTED="\$(curl -fSL "\$SUMS" | grep "agent-relay-linux-\$ARCH" | awk '{print \$1}')" || { logf "sums download failed"; result failed "sums"; exit 1; }
ACTUAL="\$(sha256sum "\$TMP/agent-relay" | awk '{print \$1}')"
[ -n "\$EXPECTED" ] && [ "\$EXPECTED" = "\$ACTUAL" ] || { logf "checksum mismatch"; result failed "checksum"; exit 1; }
logf "checksum ok"
TS="\$(date +%Y%m%d%H%M%S)"
cp "\$BIN_PATH" "\$BIN_PATH.bak.\$TS" || { logf "backup failed"; result failed "backup"; exit 1; }
install -m 755 "\$TMP/agent-relay" "\$BIN_PATH" || { logf "install failed"; result failed "install"; exit 1; }
rm -rf "\$TMP"
logf "installed, restarting"
systemctl restart agent-relay || { logf "restart failed, rolling back"; cp "\$BIN_PATH.bak.\$TS" "\$BIN_PATH"; systemctl restart agent-relay || true; result rolled_back "restart-failed"; exit 1; }
# 健康检查：读 config 找端口/模式（与 install.sh §8 同逻辑简化版）
PORT="\$(grep -E '^port = ' "$CONFIG_PATH" | awk '{print \$3}' || echo 18789)"
sleep 3
if curl -sk --max-time 5 "http://127.0.0.1:\$PORT/health" | grep -q '"ok":true'; then
  logf "health ok"
  ls -t \$BIN_PATH.bak.* 2>/dev/null | tail -n +4 | xargs -r rm -f
  result ok "\$ver"
  exit 0
fi
logf "health failed, rolling back"
cp "\$BIN_PATH.bak.\$TS" "\$BIN_PATH"
systemctl restart agent-relay || true
sleep 3
if curl -sk --max-time 5 "http://127.0.0.1:\$PORT/health" | grep -q '"ok":true'; then
  result rolled_back "health-failed"
else
  result failed "health-failed-rollback-uncertain"
fi
exit 1
} 2>&1 | tee -a "\$JOB"
EOF
  chmod 700 "$HELPER"
  chown root:root "$HELPER"
  printf 'agent-relay ALL=(root) NOPASSWD: /usr/local/sbin/agent-relay-update apply v*\n' > /etc/sudoers.d/agent-relay-update
  chmod 440 /etc/sudoers.d/agent-relay-update
  visudo -c -q || { warn "sudoers 校验失败，已删除该文件（Web 更新不可用，重跑脚本排查）"; rm -f /etc/sudoers.d/agent-relay-update; }
  log "Web 一键更新 helper 已安装（$HELPER + sudoers）"
fi

# ---- 8. 健康检查 ----
sleep 1
if [ "$LISTEN" = "auto" ]; then CHECK_HOST="127.0.0.1"; elif [ "$LISTEN" = "0.0.0.0" ]; then CHECK_HOST="127.0.0.1"; else CHECK_HOST="$LISTEN"; fi
SCHEME="http"; [ "$NET" = "public" ] && SCHEME="https"
if curl -sk --max-time 5 "$SCHEME://$CHECK_HOST:$PORT/health" | grep -q '"ok":true'; then
  log "健康检查通过（直连）：$SCHEME://$CHECK_HOST:$PORT/health"
  if [ "$NET" = "caddy" ]; then
    sleep 3 # 给 caddy 自动 HTTPS 一点拿证时间
    if curl -sk --max-time 10 "https://$PROXY_DOMAIN/health" | grep -q '"ok":true'; then
      log "反代检查通过：https://$PROXY_DOMAIN/health"
    else
      warn "经 caddy 访问失败（直连正常）：caddy 可能还在申请证书，稍后手动验证 https://$PROXY_DOMAIN/health"
    fi
  fi
else
  warn "健康检查未通过，查看日志："
  if [ "$MODE" = "docker" ]; then docker logs --tail 20 agent-relay || true
  else journalctl -u agent-relay --no-pager -n 20 || true; fi
  die "启动失败，按上面日志排查（常见：端口占用 / tailscale 未上线 / 证书路径错）"
fi

echo
log "部署完成！"
if [ "$NET" = "caddy" ]; then
  echo "  Web 控制台： https://$PROXY_DOMAIN/ （用刚才设的 admin 密码登录）"
else
  echo "  Web 控制台： $SCHEME://$CHECK_HOST:$PORT/ （用刚才设的 admin 密码登录）"
fi
[ "$GEN_PW" -eq 1 ] && echo "  随机 admin 密码： $ADMIN_PW  ← 只显示这一次，请收好"
echo "  下一步：进 Web UI → 邀请 & Prompt → 生成首个助手邀请（见 docs/ONBOARDING.md）"
echo "  升级：重跑本脚本并 --version 新版本（数据目录不动，schema 自动迁移）"
