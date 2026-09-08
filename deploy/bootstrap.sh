#!/usr/bin/env bash
# ============================================================================
# koolo-hub 服务端一键引导（在阿里云 Ubuntu 22.04 上以 root 运行）
#
# 用法（复制一行到服务器执行即可）：
#   bash <(curl -fsSL https://raw.githubusercontent.com/dpsun/koolo-hub/main/deploy/bootstrap.sh)
#
# 它会自动：
#   1. 从 GitHub Releases 下载预编译的 Linux 二进制（无需在服务器装 Go）
#   2. 下载 install.sh 并安装为 systemd 服务（koolo 账户、自动重启、每日备份）
#   3. 创建默认租户 "default" 并打印 ingest_key / view_key
#   4. 把数据库文件归属修正为 koolo 账户
#
# 完成后：
#   - 看板   : http://<服务器公网IP>:8080/?key=<view_key>
#   - 管理台 : http://<服务器公网IP>:8080/admin  （token 在 /etc/koolo-hub.env）
#   - 记得在阿里云「安全组」手动放行 TCP 8080 入方向
# ============================================================================
set -euo pipefail

REPO="dpsun/koolo-hub"
RAW="https://raw.githubusercontent.com/${REPO}/main"
PORT="${KOLOO_PORT:-8080}"
DB_PATH="/var/lib/koolo-hub/hub.db"

echo "==> 解析最新 Release 版本"
TAG="$(curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" \
        | grep -oP '"tag_name":\s*"\K[^"]+')"
echo "    版本: ${TAG}"

echo "==> 下载 install.sh"
curl -fsSL "${RAW}/deploy/install.sh" -o /tmp/koolo-install.sh
chmod +x /tmp/koolo-install.sh

echo "==> 下载预编译二进制 koolo-hub-linux-amd64"
curl -fsSL -o /tmp/koolo-hub \
  "https://github.com/${REPO}/releases/download/${TAG}/koolo-hub-linux-amd64"
chmod +x /tmp/koolo-hub

echo "==> 执行安装（端口 ${PORT}）"
PORT="${PORT}" KOLOO_BIN=/tmp/koolo-hub bash /tmp/koolo-install.sh --port "${PORT}"

echo "==> 创建默认租户 default 并生成密钥"
koolo-hub tenant add --name default --db "${DB_PATH}"

echo "==> 修正数据库文件归属（避免 koolo 服务账户无法读写）"
chown -R koolo:koolo "$(dirname "${DB_PATH}")"

echo
echo "==> 引导完成。"
echo "    看板   : http://<服务器公网IP>:${PORT}/?key=<上面的 view_key>"
echo "    管理台 : http://<服务器公网IP>:${PORT}/admin  (token 见 /etc/koolo-hub.env)"
echo "    安全组 : 请在阿里云控制台放行 TCP ${PORT} 入方向"
echo "    客户端 : 把上面的 ingest_key 填入 koolo 的 config.yaml -> telemetry.key"
