# DEPLOY — VPS 上机手册（DESIGN §10.1）

目标机器：常开 VPS，已加入 Tailscale，能 `tailscale ip -4` 拿到 100.x。

## 0. 一键安装（推荐）

```bash
curl -sSL https://raw.githubusercontent.com/AlixWang/agent-relay/main/deploy/install.sh | sudo bash
```

交互向导依次确认：版本（默认最新 Release）→ 部署方式（systemd 二进制 / docker 镜像）→
网络模式（tailscale 内网 / 公网 TLS / 仅本机 / caddy 反代共存）→ admin 密码 → 端口/数据目录。

| 网络模式 | 适用场景 | TLS 谁终止 | relay 监听 |
|---|---|---|---|
| tailscale | 无公网需求，助手全在 tailnet | 无（内网明文） | tailnet IP |
| public | 干净机器，80 空闲或已有证书 | relay 自己（certbot 申请） | 0.0.0.0:443 |
| loopback | 本机调试 | 无 | 127.0.0.1 |
| caddy | 80/443 已被 caddy 占用（如本机有 v2ray/网站） | caddy 自动 HTTPS | 127.0.0.1 明文，caddy 反代公网域名 |
脚本自动完成：SHA256 校验 → 写 config（含 bcrypt hash）→ 建用户/systemd unit 或起容器 → 健康检查。

```bash
# 无人值守示例：最新版 + systemd + tailscale + 随机密码
curl -sSL .../install.sh | sudo bash -s -- -y --random-pw
# 指定版本 + docker + 公网 TLS
curl -sSL .../install.sh | sudo bash -s -- --version v0.1.2 --mode docker \
  --net public --tls-cert /etc/letsencrypt/live/x/fullchain.pem \
  --tls-key /etc/letsencrypt/live/x/privkey.pem --admin-pw '...'
```

升级也是重跑脚本（`--version` 新版本），数据目录不动，schema 自动迁移。

以下为手动步骤（脚本行为的对照说明，出问题时排查用）。

## 1. 建用户与目录

```bash
sudo useradd -r -d /var/lib/agent-relay -s /usr/sbin/nologin agent-relay
sudo mkdir -p /var/lib/agent-relay /etc/agent-relay /opt/agent-relay
```

## 2. 装二进制（从 CI 取产物）

每次打 tag（`git tag vX.Y.Z && git push origin vX.Y.Z`）会自动：

- 构建 `agent-relay-linux-amd64` + `agent-relay-linux-arm64`（+ SHA256SUMS）并发布到 GitHub Release；
- 构建多架构镜像并推送到 `ghcr.io/alixwang/agent-relay:<tag>`（+ `latest`）。

`main` 分支每次 push 跑 CI（gofmt/vet/单测/双架构交叉编译/e2e），红了不准打 tag。

```bash
# 方式 A：systemd（推荐，单二进制 + SQLite，见 deploy/agent-relay.service）
ARCH=$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/')
VER=v0.1.0  # 改成实际 tag
curl -sSL -o /tmp/agent-relay \
  https://github.com/AlixWang/agent-relay/releases/download/$VER/agent-relay-linux-$ARCH
# 验校验和（Release 页 SHA256SUMS 里对照）
sha256sum /tmp/agent-relay
ssh vps 'sudo install -m 755 /tmp/agent-relay /usr/local/bin/agent-relay'

# 方式 B：容器（GHCR 镜像，需先 docker login ghcr.io）
# docker pull ghcr.io/alixwang/agent-relay:v0.1.0
# docker run -d --name agent-relay --restart unless-stopped \
#   -v agent-relay-data:/var/lib/agent-relay -p 18789:18789 \
#   ghcr.io/alixwang/agent-relay:v0.1.0
```

> 本地应急才手动编译：`CGO_ENABLED=0 GOOS=linux GOARCH=$ARCH go build -trimpath -o agent-relay ./cmd/agent-relay`（CGO 已禁用，modernc/sqlite 纯 Go，无需目标机工具链）。

## 3. 写配置

```bash
# 生成 admin 密码 hash（本地跑即可，不经过网络）
./agent-relay -hash 'your-strong-password'

ssh vps
sudo cp configs/config.toml.example /etc/agent-relay/config.toml
sudo $EDITOR /etc/agent-relay/config.toml
# 必改：admin_password_hash；确认：listen_addr="auto"，port=18789
```

## 4. 起服务

```bash
sudo cp deploy/agent-relay.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now agent-relay
sudo journalctl -u agent-relay -f   # 看到 listening on 100.x:18789 即成功
```

## 5. 验证

```bash
curl http://$(tailscale ip -4 | head -1):18789/health
# {"ok":true,"version":2,"protocol":1,"min_client":1}
```

浏览器打开 `http://<tailnet-ip>:18789/`，用 admin 密码登录，
走一遍 **邀请 & Prompt → 成员表**，再让第一个助手做冒烟验证。

## 6. 升级（二进制替换，数据不动）

```bash
# 从新 tag 的 GitHub Release 取对应架构二进制，校验后替换
ARCH=$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/')
VER=v0.2.0
curl -sSL -o /tmp/agent-relay \
  https://github.com/AlixWang/agent-relay/releases/download/$VER/agent-relay-linux-$ARCH
sha256sum /tmp/agent-relay   # 与 Release 页 SHA256SUMS 对照
ssh vps 'sudo install -m 755 /tmp/agent-relay /usr/local/bin/agent-relay && sudo systemctl restart agent-relay'
# 容器方式：docker pull ghcr.io/alixwang/agent-relay:v0.2.0 && docker rm -f agent-relay && 按 §2 方式 B 重起（数据在 volume 里不动）
```

Schema 迁移内嵌启动时执行（`internal/store/schema.sql` + 老库 FK 重建），
数据文件格式 v1 内稳定，无需手动迁移。

## 7. 备份

```bash
# SQLite 单文件 + 归档目录，停服拷走即可（WAL 模式下三个文件一起拷）
sudo systemctl stop agent-relay
sudo tar czf agent-relay-backup-$(date +%F).tgz -C /var/lib agent-relay
sudo systemctl start agent-relay
```

归档 JSONL 在 `/var/lib/agent-relay/archive/`，审计导出走 UI
（审计 & Token → 导出 JSON），线程导出走线程查看器。

## 8. 回滚到 Python v2

见 `docs/MIGRATION.md` 回滚节：`systemctl restart muse-relay` 即可，
`prototype/` 保留原 relay.py + service 文件。
