#!/usr/bin/env bash
# 在【你的本机】Git Bash 中执行（不是 WorkBuddy 聊天框）。
# 前置：本机能 ssh 到服务器（你已验证热点可连）。会提示输入 root 密码两次。
set -euo pipefail

SERVER="root@8.133.228.22"
REMOTE_DIR="/tmp/kd"
PORT=8080
# 脚本位于 koolo-hub/deploy/，上一级即项目根
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
LOCAL_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"

echo "==> [1/3] 上传二进制与安装脚本到 $SERVER:$REMOTE_DIR"
ssh "$SERVER" "mkdir -p $REMOTE_DIR"
scp "$LOCAL_DIR/dist/koolo-hub" "$LOCAL_DIR/deploy/install.sh" "$SERVER:$REMOTE_DIR/"

echo "==> [2/3] 服务器上执行安装（会打印 admin-token，请抄下来）"
ssh -t "$SERVER" "cd $REMOTE_DIR && bash install.sh --port $PORT"

echo "==> [3/3] 创建默认租户（以 koolo 用户执行，保证数据库归属正确）"
ssh -t "$SERVER" "systemctl start koolo-hub 2>/dev/null; runuser -u koolo -- /usr/local/bin/koolo-hub tenant add --name 默认租户 --db /var/lib/koolo-hub/hub.db"

echo
echo "==================== 完成 ===================="
echo " 手机看板 : http://8.133.228.22:$PORT/        （view_key 见上方 tenant 输出）"
echo " 管理台   : http://8.133.228.22:$PORT/admin    （admin-token 见 install.sh 输出 / /etc/koolo-hub.env）"
echo " 客户端   : config.yaml 填 ingest_key（见上方 tenant 输出）+ endpoint=http://8.133.228.22:$PORT/api/v1/ingest"
echo
echo " 别忘了：阿里云控制台安全组放行 TCP $PORT 入方向；并改掉 root 密码 / 配 SSH key。"
