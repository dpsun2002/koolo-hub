#!/usr/bin/env bash
# koolo-hub 服务端一键安装（Ubuntu 22.04 / Debian 12，root 运行）
# 用法（在被控机执行）：
#   bash install.sh [--db /var/lib/koolo-hub/hub.db] [--addr :8080]
#
# 完成：服务账户、二进制放置、admin token 生成(600)、systemd(Restart=always)、
#       每日 04:10 备份(保留14天)、防火墙放行。
# 安全：admin token 等同数据库可读权限，勿外泄；写操作需 --allow-write 启动。

set -euo pipefail

# 从仓库现拉源码编译（当本机无法传二进制时用）：
#   KOLOO_REPO=https://gitee.com/你的名/koolo-hub bash install.sh
# 国内服务器优先 gitee.com；如需代理给 GO_PROXY / git 配置。
build_from_repo(){
  local repo="$1"
  echo "==> 从仓库构建: $repo"
  # 1) 装 Go（国内镜像优先）
  if ! command -v go >/dev/null 2>&1; then
    local ver="1.23.4" arch="amd64"
    local url="https://golang.google.cn/dl/go${ver}.linux-${arch}.tar.gz"
    command -v curl >/dev/null 2>&1 || { echo "需要 curl" >&2; exit 1; }
    curl -fsSL "$url" -o /tmp/go.tgz || curl -fsSL "https://go.dev/dl/go${ver}.linux-${arch}.tar.gz" -o /tmp/go.tgz
    tar -C /usr/local -xzf /tmp/go.tgz
    export PATH=$PATH:/usr/local/go/bin
  fi
  export GOFLAGS=-mod=mod
  local tmp; tmp="$(mktemp -d)"
  git clone --depth 1 "$repo" "$tmp/src" 2>/dev/null || git clone "$repo" "$tmp/src"
  cd "$tmp/src"
  go build -o /tmp/koolo-hub .
  cd /
  echo "/tmp/koolo-hub"
}

BIN="/usr/local/bin/koolo-hub"
BIN_SRC="${KOLOO_BIN:-./koolo-hub}"   # 二进制来源：默认同目录；可用环境变量指定
DB_PATH="/var/lib/koolo-hub/hub.db"
ADDR=":8080"
ENV_FILE="/etc/koolo-hub.env"
BACKUP_DIR="/var/backups/koolo-hub"
ALLOW_WRITE=""

while [[ $# -gt 0 ]]; do
  case "$1" in
    --db) DB_PATH="$2"; shift 2;;
    --addr) ADDR="$2"; shift 2;;
    --port) ADDR=":$2"; shift 2;;
    --allow-write) ALLOW_WRITE="--allow-write"; shift;;
    *) echo "未知参数: $1" >&2; exit 2;;
  esac
done

PORT="${ADDR#:}"
[[ "$PORT" =~ ^[0-9]+$ ]] || { echo "端口解析失败: $ADDR" >&2; exit 2; }

# 没给本地二进制，但给了仓库 → 现拉编译
if [[ ! -f "$BIN_SRC" ]] && [[ -n "${KOLOO_REPO:-}" ]]; then
  BIN_SRC="$(build_from_repo "$KOLOO_REPO")"
fi
[[ -f "$BIN_SRC" ]] || { echo "找不到二进制: $BIN_SRC（可设 KOLOO_BIN=路径，或 KOLOO_REPO=git地址 现拉编译）" >&2; exit 1; }

echo "==> 安装 koolo-hub 到 $BIN"
install -m 0755 "$BIN_SRC" "$BIN"

echo "==> 创建服务账户 koolo"
id -u koolo >/dev/null 2>&1 || useradd -r -s /usr/sbin/nologin koolo
install -d -o koolo -g koolo -m 0700 "$(dirname "$DB_PATH")"
install -d -o koolo -g koolo -m 0700 "$BACKUP_DIR"

echo "==> 生成 admin token 并写入 $ENV_FILE (chmod 600)"
if [[ -f "$ENV_FILE" ]]; then
  echo "   已存在 $ENV_FILE，将保留原 token"
else
  ADMIN_TOKEN="$(head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n')"
  printf 'KOLOO_DB=%s\nKOLOO_ADDR=%s\nKOLOO_ADMIN_TOKEN=%s\n' "$DB_PATH" "$ADDR" "$ADMIN_TOKEN" > "$ENV_FILE"
  chmod 600 "$ENV_FILE"
  echo "   新 token: $ADMIN_TOKEN   （请立即记录，/admin 查询台需要）"
fi

echo "==> 写入 systemd 单元"
cat >/etc/systemd/system/koolo-hub.service <<EOF
[Unit]
Description=koolo-hub fleet telemetry server
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=koolo
Group=koolo
EnvironmentFile=$ENV_FILE
ExecStart=$BIN serve --db \$KOLOO_DB --addr \$KOLOO_ADDR --admin-token \$KOLOO_ADMIN_TOKEN $ALLOW_WRITE
Restart=always
RestartSec=3
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=$(dirname "$DB_PATH") $BACKUP_DIR

[Install]
WantedBy=multi-user.target
EOF

systemctl daemon-reload
systemctl enable --now koolo-hub

echo "==> 配置每日备份 (cron 04:10，保留14天)"
if command -v sqlite3 >/dev/null 2>&1; then
  BAK="10 4 * * * koolo sqlite3 $DB_PATH \".backup $BACKUP_DIR/hub-\$(date +\\%F).db\" 2>/dev/null || true"
else
  BAK="10 4 * * * koolo cp $DB_PATH $BACKUP_DIR/hub-\$(date +\\%F).db 2>/dev/null || true"
fi
printf '%s\n24 4 * * * koolo find %s -name "hub-*.db" -mtime +14 -delete 2>/dev/null || true\n' "$BAK" "$BACKUP_DIR" >/etc/cron.d/koolo-hub-backup

if command -v ufw >/dev/null 2>&1; then
  echo "==> 放行端口 $PORT (ufw)"
  ufw allow "$PORT"/tcp >/dev/null 2>&1 || true
fi

echo
echo "==> 安装完成。"
echo "    状态   : systemctl status koolo-hub"
echo "    看板   : http://<本机IP>:$PORT/"
echo "    管理台 : http://<本机IP>:$PORT/admin  (token 见 $ENV_FILE)"
echo "    数据库 : $DB_PATH"
echo "    备份   : $BACKUP_DIR"
echo
echo "    若使用阿里云：请到控制台安全组手动放行 TCP $PORT（入方向）。"
echo "    建议禁用 root 密码登录、改用 SSH key（参见 README 安全章节）。"
