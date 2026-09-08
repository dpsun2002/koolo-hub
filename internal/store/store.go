package store

import (
	"database/sql"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

// CST 固定 +08:00：服务端不依赖系统 tzdata，"今日"边界按客户端本地自然日解释。
var CST = time.FixedZone("CST", 8*3600)

const schema = `
PRAGMA journal_mode=WAL;
PRAGMA synchronous=NORMAL;

CREATE TABLE IF NOT EXISTS tenants(
  id          TEXT PRIMARY KEY,
  name        TEXT NOT NULL,
  ingest_key  TEXT NOT NULL UNIQUE,
  view_key    TEXT NOT NULL UNIQUE,
  created_at  INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS machines(
  tenant_id   TEXT NOT NULL,
  machine_id  TEXT NOT NULL,
  alias       TEXT NOT NULL DEFAULT '',
  last_seen   INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY(tenant_id, machine_id)
);

-- 角色主键 = (tenant_id, char_name)：同名角色跨机器算一个，符合"按角色名归类"。
CREATE TABLE IF NOT EXISTS characters(
  tenant_id       TEXT NOT NULL,
  char_name       TEXT NOT NULL,
  class           TEXT NOT NULL DEFAULT '',
  last_machine_id TEXT NOT NULL DEFAULT '',
  last_seen       INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY(tenant_id, char_name)
);

-- 所有事件一张表，uuid 主键天然去重，断线重放不会产生重复计数。
CREATE TABLE IF NOT EXISTS events(
  uuid       TEXT PRIMARY KEY,
  tenant_id  TEXT NOT NULL,
  char_name  TEXT NOT NULL,
  day        TEXT NOT NULL,
  type       TEXT NOT NULL,
  result     TEXT NOT NULL DEFAULT '',
  run        TEXT NOT NULL DEFAULT '',
  item       TEXT NOT NULL DEFAULT '',
  quality    TEXT NOT NULL DEFAULT '',
  dur_ms     INTEGER NOT NULL DEFAULT 0,
  ts         INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_events_char_day ON events(tenant_id, char_name, day);
CREATE INDEX IF NOT EXISTS idx_events_day_type ON events(tenant_id, day, type);

-- 工作时段（session 起止），今日时长由它与 [当日0点, 次日0点) 求交集得出。
CREATE TABLE IF NOT EXISTS sessions(
  uuid      TEXT PRIMARY KEY,
  tenant_id TEXT NOT NULL,
  char_name TEXT NOT NULL,
  start_ts  INTEGER NOT NULL,
  end_ts    INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_sessions_char ON sessions(tenant_id, char_name);

-- 实时快照，一角色一行
CREATE TABLE IF NOT EXISTS heartbeats(
  tenant_id     TEXT NOT NULL,
  char_name     TEXT NOT NULL,
  machine_id    TEXT NOT NULL DEFAULT '',
  instance_id   TEXT NOT NULL DEFAULT '',
  class         TEXT NOT NULL DEFAULT '',
  status        TEXT NOT NULL DEFAULT '',
  status_until  INTEGER NOT NULL DEFAULT 0,
  current_run   TEXT NOT NULL DEFAULT '',
  session_start INTEGER NOT NULL DEFAULT 0,
  version       TEXT NOT NULL DEFAULT '',
  ts            INTEGER NOT NULL,
  PRIMARY KEY(tenant_id, char_name)
);

-- 按天物化表：由 events/sessions 重算得出，不做增量累加（保证重放可自愈）
CREATE TABLE IF NOT EXISTS daily(
  tenant_id  TEXT NOT NULL,
  char_name  TEXT NOT NULL,
  day        TEXT NOT NULL,
  work_sec   INTEGER NOT NULL DEFAULT 0,
  runs_ok    INTEGER NOT NULL DEFAULT 0,
  runs_fail  INTEGER NOT NULL DEFAULT 0,
  picks      INTEGER NOT NULL DEFAULT 0,
  deaths     INTEGER NOT NULL DEFAULT 0,
  chickens   INTEGER NOT NULL DEFAULT 0,
  errors     INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY(tenant_id, char_name, day)
);
CREATE INDEX IF NOT EXISTS idx_daily_char ON daily(tenant_id, char_name);
`

type Store struct {
	db *sql.DB
}

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // SQLite 单写者，串行化避免 SQLITE_BUSY

	if _, err = db.Exec(schema); err != nil {
		return nil, fmt.Errorf("建表失败: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }
func (s *Store) DB() *sql.DB  { return s.db }

/* ---------------- 租户 ---------------- */

type Tenant struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	IngestKey string `json:"ingest_key"`
	ViewKey   string `json:"view_key"`
	CreatedAt int64  `json:"created_at"`
}

func (s *Store) CreateTenant(t Tenant) error {
	_, err := s.db.Exec(`INSERT INTO tenants(id,name,ingest_key,view_key,created_at) VALUES(?,?,?,?,?)`,
		t.ID, t.Name, t.IngestKey, t.ViewKey, t.CreatedAt)
	return err
}

func (s *Store) ListTenants() ([]Tenant, error) {
	rows, err := s.db.Query(`SELECT id,name,ingest_key,view_key,created_at FROM tenants ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Tenant
	for rows.Next() {
		var t Tenant
		if err := rows.Scan(&t.ID, &t.Name, &t.IngestKey, &t.ViewKey, &t.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) TenantByIngestKey(k string) (*Tenant, error) { return s.tenantBy("ingest_key", k) }
func (s *Store) TenantByViewKey(k string) (*Tenant, error)   { return s.tenantBy("view_key", k) }

func (s *Store) tenantBy(col, k string) (*Tenant, error) {
	var t Tenant
	err := s.db.QueryRow(
		fmt.Sprintf(`SELECT id,name,ingest_key,view_key,created_at FROM tenants WHERE %s=?`, col), k).
		Scan(&t.ID, &t.Name, &t.IngestKey, &t.ViewKey, &t.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

/* ---------------- 写入 ---------------- */

type Event struct {
	UUID    string `json:"uuid"`
	Type    string `json:"type"`
	TS      int64  `json:"ts"`
	Day     string `json:"day"`
	Char    string `json:"char"`
	Result  string `json:"result"`
	Run     string `json:"run"`
	Item    string `json:"item"`
	Quality string `json:"quality"`
	DurMS   int64  `json:"dur_ms"`
}

type Session struct {
	UUID    string `json:"uuid"`
	StartTS int64  `json:"start_ts"`
	EndTS   int64  `json:"end_ts"`
}

// SaveEvents 批量落库，靠 uuid 主键去重。返回真正新增的条数。
func (s *Store) SaveEvents(tenantID, defaultChar string, evs []Event, sess []Session) (int, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	evStmt, err := tx.Prepare(`INSERT OR IGNORE INTO events
		(uuid,tenant_id,char_name,day,type,result,run,item,quality,dur_ms,ts)
		VALUES(?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return 0, err
	}
	defer evStmt.Close()

	sessStmt, err := tx.Prepare(`INSERT OR IGNORE INTO sessions(uuid,tenant_id,char_name,start_ts,end_ts) VALUES(?,?,?,?,?)`)
	if err != nil {
		return 0, err
	}
	defer sessStmt.Close()

	added := 0
	for _, e := range evs {
		if e.UUID == "" || e.Type == "" {
			continue
		}
		char := e.Char
		if char == "" {
			char = defaultChar
		}
		if char == "" {
			continue
		}
		res, err := evStmt.Exec(e.UUID, tenantID, char, e.Day, e.Type, e.Result, e.Run, e.Item, e.Quality, e.DurMS, e.TS)
		if err != nil {
			return 0, err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			added++
		}
	}
	for _, x := range sess {
		if x.UUID == "" || x.EndTS <= x.StartTS {
			continue
		}
		if _, err := sessStmt.Exec(x.UUID, tenantID, defaultChar, x.StartTS, x.EndTS); err != nil {
			return 0, err
		}
	}

	return added, tx.Commit()
}

type Heartbeat struct {
	MachineID    string `json:"machine_id"`
	InstanceID   string `json:"instance_id"`
	Char         string `json:"char"`
	Class        string `json:"class"`
	Status       string `json:"status"`
	StatusUntil  int64  `json:"status_until"`
	CurrentRun   string `json:"run"`
	SessionStart int64  `json:"session_start"`
	Day          string `json:"day"`
	TS           int64  `json:"ts"`
	Version      string `json:"ver"`
}

func (s *Store) SaveHeartbeat(tenantID string, h Heartbeat) error {
	if h.Char == "" {
		return nil
	}
	if h.TS == 0 {
		h.TS = time.Now().UnixMilli()
	}
	now := time.Now().UnixMilli()

	if _, err := s.db.Exec(`INSERT INTO machines(tenant_id,machine_id,last_seen) VALUES(?,?,?)
		ON CONFLICT(tenant_id,machine_id) DO UPDATE SET last_seen=excluded.last_seen`,
		tenantID, h.MachineID, now); err != nil {
		return err
	}

	_, err := s.db.Exec(`INSERT INTO characters(tenant_id,char_name,class,last_machine_id,last_seen)
		VALUES(?,?,?,?,?)
		ON CONFLICT(tenant_id,char_name) DO UPDATE SET
		  last_machine_id=excluded.last_machine_id,
		  last_seen=excluded.last_seen,
		  class=CASE WHEN excluded.class<>'' THEN excluded.class ELSE characters.class END`,
		tenantID, h.Char, h.Class, h.MachineID, now)
	if err != nil {
		return err
	}

	_, err = s.db.Exec(`INSERT INTO heartbeats
		(tenant_id,char_name,machine_id,instance_id,class,status,status_until,current_run,session_start,version,ts)
		VALUES(?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(tenant_id,char_name) DO UPDATE SET
		  machine_id=excluded.machine_id, instance_id=excluded.instance_id,
		  class=CASE WHEN excluded.class<>'' THEN excluded.class ELSE heartbeats.class END,
		  status=excluded.status, status_until=excluded.status_until,
		  current_run=excluded.current_run, session_start=excluded.session_start,
		  version=excluded.version, ts=excluded.ts`,
		tenantID, h.Char, h.MachineID, h.InstanceID, h.Class, h.Status,
		h.StatusUntil, h.CurrentRun, h.SessionStart, h.Version, h.TS)
	return err
}

/* ---------------- 聚合 ---------------- */

type DayAgg struct {
	WorkSec  int64 `json:"work_sec"`
	RunsOK   int   `json:"runs_ok"`
	RunsFail int   `json:"runs_fail"`
	Picks    int   `json:"picks"`
	Deaths   int   `json:"deaths"`
	Chickens int   `json:"chickens"`
	Errors   int   `json:"errors"`
}

// AggregateDay 统计某角色某天的聚合值。work_sec 由 closed sessions 与当日区间求交集，
// 再加上"进行中"的那一段（心跳里的 session_start），因此进程被强杀也只丢最后一次心跳之后的时间。
func (s *Store) AggregateDay(tenantID, char, day string) (DayAgg, error) {
	var a DayAgg
	start, end, err := dayRange(day)
	if err != nil {
		return a, err
	}

	rows, err := s.db.Query(`SELECT start_ts,end_ts FROM sessions WHERE tenant_id=? AND char_name=? AND end_ts>? AND start_ts<?`,
		tenantID, char, start, end)
	if err != nil {
		return a, err
	}
	var s1, e1 int64
	for rows.Next() {
		if err := rows.Scan(&s1, &e1); err != nil {
			rows.Close()
			return a, err
		}
		a.WorkSec += overlap(s1, e1, start, end) / 1000
	}
	rows.Close()

	// 进行中的一段
	var sessionStart int64
	err = s.db.QueryRow(`SELECT COALESCE(session_start,0) FROM heartbeats WHERE tenant_id=? AND char_name=?`, tenantID, char).Scan(&sessionStart)
	if err != nil && err != sql.ErrNoRows {
		return a, err
	}
	if sessionStart > 0 {
		now := time.Now().UnixMilli()
		a.WorkSec += overlap(sessionStart, now, start, end) / 1000
	}

	err = s.db.QueryRow(`
		SELECT
		  COALESCE(SUM(CASE WHEN type='run_end' AND result='kill' THEN 1 ELSE 0 END),0),
		  COALESCE(SUM(CASE WHEN type='run_end' AND result<>'kill' AND result<>'' THEN 1 ELSE 0 END),0),
		  COALESCE(SUM(CASE WHEN type='item' THEN 1 ELSE 0 END),0),
		  COALESCE(SUM(CASE WHEN result='death' THEN 1 ELSE 0 END),0),
		  COALESCE(SUM(CASE WHEN result IN ('chicken','merc_chicken') THEN 1 ELSE 0 END),0),
		  COALESCE(SUM(CASE WHEN result='error' THEN 1 ELSE 0 END),0)
		FROM events WHERE tenant_id=? AND char_name=? AND day=?`,
		tenantID, char, day).Scan(&a.RunsOK, &a.RunsFail, &a.Picks, &a.Deaths, &a.Chickens, &a.Errors)
	return a, err
}

func (s *Store) RunsByTaskToday(tenantID, char, day string) (map[string]int, error) {
	out := map[string]int{}
	rows, err := s.db.Query(
		`SELECT COALESCE(run,'') FROM events WHERE tenant_id=? AND char_name=? AND day=? AND type='run_end' AND result='kill'`,
		tenantID, char, day)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var r string
		if err := rows.Scan(&r); err != nil {
			return nil, err
		}
		if r == "" {
			r = "unknown"
		}
		out[r]++
	}
	return out, rows.Err()
}

func (s *Store) ItemsToday(tenantID, char, day string, limit int) ([]ItemRow, error) {
	rows, err := s.db.Query(
		`SELECT COALESCE(item,''),COALESCE(quality,''),ts FROM events
		 WHERE tenant_id=? AND char_name=? AND day=? AND type='item' ORDER BY ts DESC LIMIT ?`,
		tenantID, char, day, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ItemRow
	for rows.Next() {
		var it ItemRow
		if err := rows.Scan(&it.Name, &it.Quality, &it.TS); err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

type ItemRow struct {
	Name    string `json:"name"`
	Quality string `json:"quality"`
	TS      int64  `json:"ts"`
}

type Totals struct {
	WorkSec int64 `json:"work_sec"`
	RunsOK  int   `json:"runs_ok"`
	Picks   int   `json:"picks"`
	Days    int   `json:"days"`
}

// Materialize 重算指定角色最近 n 天的 daily 表。不做增量累加，保证重放/修复后可自愈。
func (s *Store) Materialize(tenantID, char string, days int) error {
	base := time.Now().In(CST)
	for i := 0; i < days; i++ {
		day := base.AddDate(0, 0, -i).Format("2006-01-02")
		a, err := s.AggregateDay(tenantID, char, day)
		if err != nil {
			return err
		}
		if _, err := s.db.Exec(`INSERT INTO daily(tenant_id,char_name,day,work_sec,runs_ok,runs_fail,picks,deaths,chickens,errors)
			VALUES(?,?,?,?,?,?,?,?,?,?)
			ON CONFLICT(tenant_id,char_name,day) DO UPDATE SET
			  work_sec=excluded.work_sec, runs_ok=excluded.runs_ok, runs_fail=excluded.runs_fail,
			  picks=excluded.picks, deaths=excluded.deaths, chickens=excluded.chickens, errors=excluded.errors`,
			tenantID, char, day, a.WorkSec, a.RunsOK, a.RunsFail, a.Picks, a.Deaths, a.Chickens, a.Errors); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) ActiveCharacters(sinceMs int64) ([]struct {
	TenantID string
	Char     string
}, error) {
	rows, err := s.db.Query(`SELECT tenant_id,char_name FROM heartbeats WHERE ts>=?`, sinceMs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []struct {
		TenantID string
		Char     string
	}
	for rows.Next() {
		var v struct {
			TenantID string
			Char     string
		}
		if err := rows.Scan(&v.TenantID, &v.Char); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

type DayPoint struct {
	Day     string `json:"day"`
	WorkSec int64  `json:"work_sec"`
	RunsOK  int    `json:"runs_ok"`
	Picks   int    `json:"picks"`
}

// DailySeries 读取物化表，供看板画近 N 日趋势（由后台 Materialize 定期刷新）。
func (s *Store) DailySeries(tenantID, char string, days int) ([]DayPoint, error) {
	limit := time.Now().In(CST).AddDate(0, 0, -(days - 1)).Format("2006-01-02")
	rows, err := s.db.Query(
		`SELECT day, work_sec, runs_ok, picks FROM daily
		 WHERE tenant_id=? AND char_name=? AND day>=? ORDER BY day`, tenantID, char, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []DayPoint
	for rows.Next() {
		var p DayPoint
		if err := rows.Scan(&p.Day, &p.WorkSec, &p.RunsOK, &p.Picks); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) TotalsAllTime(tenantID, char string) (Totals, error) {
	var t Totals
	err := s.db.QueryRow(`SELECT
		COALESCE(SUM(work_sec),0), COALESCE(SUM(runs_ok),0), COALESCE(SUM(picks),0), COUNT(*)
		FROM daily WHERE tenant_id=? AND char_name=?`, tenantID, char).
		Scan(&t.WorkSec, &t.RunsOK, &t.Picks, &t.Days)
	if err != nil {
		return t, err
	}

	// 今日通常还没被物化（后台每 3 分钟跑一次），直接用实时值补上，
	// 否则新角色第一天"历史累计"会一直是 0。
	today := time.Now().In(CST).Format("2006-01-02")
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM daily WHERE tenant_id=? AND char_name=? AND day=?`, tenantID, char, today).Scan(&n); err != nil {
		return t, err
	}
	if n == 0 {
		a, err := s.AggregateDay(tenantID, char, today)
		if err != nil {
			return t, err
		}
		t.WorkSec += a.WorkSec
		t.RunsOK += a.RunsOK
		t.Picks += a.Picks
		t.Days++
	}
	return t, nil
}

// Purge 清理过期明细，daily 聚合永久保留。
func (s *Store) Purge(days int) error {
	cut := time.Now().AddDate(0, 0, -days).UnixMilli()
	if _, err := s.db.Exec(`DELETE FROM events WHERE ts<?`, cut); err != nil {
		return err
	}
	_, err := s.db.Exec(`DELETE FROM sessions WHERE end_ts<?`, cut)
	return err
}

/* ---------------- 管理查询（仅服务端本地/管理员使用） ---------------- */

// TableInfo 单表元信息
type TableInfo struct {
	Name string `json:"name"`
	Rows int64  `json:"rows"`
	SQL  string `json:"sql"`
}

// ListTables 列出所有业务表及其行数、建表语句。
func (s *Store) ListTables() ([]TableInfo, error) {
	rows, err := s.db.Query(`SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		return nil, err
	}
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return nil, err
		}
		names = append(names, n)
	}
	rows.Close()

	out := make([]TableInfo, 0, len(names))
	for _, n := range names {
		var cnt int64
		_ = s.db.QueryRow(fmt.Sprintf(`SELECT COUNT(*) FROM "%s"`, n)).Scan(&cnt)
		var ddl string
		_ = s.db.QueryRow(`SELECT sql FROM sqlite_master WHERE name=?`, n).Scan(&ddl)
		out = append(out, TableInfo{Name: n, Rows: cnt, SQL: ddl})
	}
	return out, nil
}

// QueryResult 执行结果
type QueryResult struct {
	Columns   []string `json:"columns"`
	Rows      [][]any  `json:"rows"`
	ElapsedMS int64    `json:"elapsed_ms"`
	Truncated bool     `json:"truncated"`
	RowCount  int      `json:"row_count"`
}

// Query 执行一条只读/管理 SQL（调用方负责权限与语句白名单校验）。
func (s *Store) Query(sql string, limit int) (QueryResult, error) {
	var res QueryResult
	start := time.Now()
	rows, err := s.db.Query(sql)
	if err != nil {
		return res, err
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		return res, err
	}
	res.Columns = cols

	for rows.Next() {
		if limit > 0 && len(res.Rows) >= limit {
			res.Truncated = true
			break
		}
		cells := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range cells {
			ptrs[i] = &cells[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return res, err
		}
		for i, v := range cells {
			switch t := v.(type) {
			case []byte:
				cells[i] = string(t)
			}
		}
		res.Rows = append(res.Rows, cells)
	}
	if err := rows.Err(); err != nil {
		return res, err
	}
	res.RowCount = len(res.Rows)
	res.ElapsedMS = time.Since(start).Milliseconds()
	return res, nil
}

/* ---------------- 工具 ---------------- */

func dayRange(day string) (int64, int64, error) {
	t, err := time.ParseInLocation("2006-01-02", day, CST)
	if err != nil {
		return 0, 0, fmt.Errorf("bad day %q: %w", day, err)
	}
	s := t.UnixMilli()
	return s, t.AddDate(0, 0, 1).UnixMilli(), nil
}

func overlap(a1, a2, b1, b2 int64) int64 {
	s, e := a1, a2
	if b1 > s {
		s = b1
	}
	if b2 < e {
		e = b2
	}
	if e <= s {
		return 0
	}
	return e - s
}
