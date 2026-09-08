# koolo-hub

koolo 多机集中监控服务端：单二进制 + SQLite + 内嵌手机看板。

## 本地构建

```bash
go build -o dist/koolo-hub .                          # 当前平台
GOOS=linux GOARCH=amd64 go build -o dist/koolo-hub .  # 部署到服务器用这个
```

## 运行

```bash
koolo-hub serve --addr :8080 --db /var/lib/koolo-hub/hub.db
```

## 发放租户（手动发 key）

```bash
koolo-hub tenant add --name "张三" --db /var/lib/koolo-hub/hub.db
```

输出两个密钥，区别很重要：

| 密钥 | 用途 | 能不能给别人 |
|---|---|---|
| `ingest_key` | 客户端上报用，填进 koolo 的 `config.yaml` → `telemetry.key` | 只能给本人 |
| `view_key` | 看板查看用，拼在 URL 后面 `/?key=xxx` | 可以分享 |

看法：`koolo-hub tenant list --db <path>`

## 客户端配置（koolo 侧）

`config/config.yaml`：

```yaml
telemetry:
  enabled: true
  endpoint: 'http://<服务器地址>:8080/api/v1/ingest'
  key: '<ingest_key>'
  machineName: ''      # 可选，机器别名
  characterName: ''    # 可选，角色名兜底
```

## 数据模型

```
tenant(用户) → character(角色, 主键 tenant_id+char_name) → events / sessions / daily
                     ↑
                  machine(机器, 仅用于展示与定位)
```

- 同名角色跨机器算**同一个角色**（符合"按角色名归类"）。
- 一机多开：每个 koolo 进程一个角色，各自独立上报；`machine_id` 默认取主机名，
  可用 `telemetry.machineName` 覆盖。

## 上报协议

客户端 → 服务端只有一个端点 `POST /api/v1/ingest`（Header `X-Koolo-Key: <ingest_key>`）：

```json
{
  "heartbeat": {"machine_id":"PC-01","char":"锤子","class":"hammerdin",
                "status":"working","run":"summoner","session_start":1788832730000,
                "day":"2026-09-08","ts":1788836330000,"ver":"0.4.0-beta1"},
  "events": [{"uuid":"...","type":"run_end","result":"kill","run":"summoner",
              "day":"2026-09-08","ts":1788836330000}],
  "sessions":[{"uuid":"...","start_ts":1788832730000,"end_ts":1788836330000}]
}
```

- `type`：`run_end`（`result` = kill / death / chicken / merc_chicken / error）、`item`
- `day` 由客户端按本地时区算好，服务端不转换时区
- 每条事件带 `uuid`，服务端按 uuid 去重 → **断线重放不会产生重复计数**

## 关键设计

- **`daily` 表是重算而非累加**：后台每 3 分钟从明细重算近 7 天，重放出问题可自愈。
- **今日时长 = Σ(工作时段 ∩ [当日0点, 次日0点))**：由 `sessions` 闭区间加上心跳里的
  `session_start`（进行中的那段）算出，不用心跳累加，进程被强杀也只丢最后一次心跳之后的时间。
- **SQLite 单连接**（`SetMaxOpenConns(1)`）：任何嵌套查询都必须先把游标读完再关，否则死锁。

## 安全

- `ingest_key` 与 `view_key` 分离；分享只给 `view_key`。
- 服务端**不存储**任何游戏账号密码。
- 面上公网务必套 HTTPS/反代；最小暴露面是只开一个端口。

## 数据库查询台（/admin）

服务端内置只读 SQL 控制台，便于你自己排查数据，无需登录服务器。

```bash
# 启动时必须带 admin token（不给则自动生成并打印在日志里）
koolo-hub serve --addr :8080 --db /var/lib/koolo-hub/hub.db --admin-token "<随机串>"
# 或环境变量： KOLOO_ADMIN_TOKEN=xxx
# 需执行写操作（DELETE/UPDATE 等）时追加 --allow-write（默认仅只读）
```

浏览器打开 `http://<服务器>:8080/admin?token=<admin token>`，可：
- 查看所有表结构与行数
- 执行 `SELECT / WITH / EXPLAIN` 只读查询
- 一键导出 CSV

**防护**：未带正确 token 一律 401；默认禁止写语句（防误操作）；SQL 禁止多语句堆叠，杜绝注入。

常用查询示例：
```sql
-- 今日各角色完成数
SELECT char_name, SUM(runs_ok), SUM(picks), SUM(deaths) FROM daily WHERE day='2026-09-08' GROUP BY char_name;
-- 原始事件明细（近7天，最多200行）
SELECT ts, char_name, type, result, run, item FROM events WHERE day>='2026-09-01' LIMIT 200;
```

## 部署到服务器（一键）

### 路线 A：GitHub 一行命令（推荐，服务器无需装 Go）

代码已发布在 GitHub，服务器只需联网拉取预编译二进制：

```bash
# 在阿里云 Ubuntu 上以 root 执行这一行：
bash <(curl -fsSL https://raw.githubusercontent.com/dpsun/koolo-hub/main/deploy/bootstrap.sh)
```

脚本会自动下载二进制 + install.sh、注册 systemd 服务、创建默认租户并打印
`ingest_key` / `view_key`。完成后记得在阿里云「安全组」放行 TCP 8080。

### 路线 B：自带二进制

```bash
# 本机先交叉编译
GOOS=linux GOARCH=amd64 go build -o dist/koolo-hub .

# 把二进制传上服务器后执行
KOLOO_BIN=/root/koolo-hub bash install.sh --port 8080
```
`install.sh` 会：创建 `koolo` 服务账户、放二进制、生成 admin token（权限 600）、
注册 systemd（`Restart=always`）、配置每日 04:10 备份（保留 14 天）、放行防火墙端口。
请勿以 root 长期运行服务；阿里云还需在**安全组**手动放行对应 TCP 端口。

## 看板二次开发

- 看板页面：`internal/web/index.html`（单文件，内嵌进二进制，`go:embed internal/web`）。
- 管理台页面：`internal/web/admin.html`。
- 改完前端后重新 `go build` 即可生效，无需单独部署前端。

