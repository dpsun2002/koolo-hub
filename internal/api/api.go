package api

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"sort"
	"time"

	"koolo-hub/internal/store"
)

var classLabel = map[string]string{
	"sorceress": "法师",
	"lightning": "电法",
	"hammerdin": "锤丁",
	"paladin":   "圣骑(练级)",
	"":          "未设置",
}

func ClassLabel(c string) string {
	if v, ok := classLabel[c]; ok {
		return v
	}
	return c
}

const onlineWindow = 90 * time.Second

type Handler struct {
	st *store.Store
}

func New(st *store.Store) *Handler { return &Handler{st: st} }

/* ---------------- 上报 ---------------- */

type IngestRequest struct {
	Heartbeat *store.Heartbeat `json:"heartbeat"`
	Events    []store.Event    `json:"events"`
	Sessions  []store.Session  `json:"sessions"`
}

func (h *Handler) Ingest(w http.ResponseWriter, r *http.Request) {
	key := r.Header.Get("X-Koolo-Key")
	if key == "" {
		key = r.URL.Query().Get("key")
	}
	t, err := h.st.TenantByIngestKey(key)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, "bad ingest key")
		return
	}
	if r.Body == nil {
		writeErr(w, http.StatusBadRequest, "empty body")
		return
	}
	defer r.Body.Close()

	var req IngestRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json: "+err.Error())
		return
	}

	char := ""
	if req.Heartbeat != nil {
		char = req.Heartbeat.Char
		if err := h.st.SaveHeartbeat(t.ID, *req.Heartbeat); err != nil {
			log.Printf("save heartbeat: %v", err)
		}
	}
	added, err := h.st.SaveEvents(t.ID, char, req.Events, req.Sessions)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, map[string]any{"accepted": added, "skipped": len(req.Events) - added})
}

/* ---------------- 看板 ---------------- */

type CharCard struct {
	Char       string           `json:"char"`
	Class      string           `json:"class"`
	ClassLabel string           `json:"class_label"`
	Machine    string           `json:"machine"`
	Online     bool             `json:"online"`
	Status     string           `json:"status"`
	StatusText string           `json:"status_text"`
	Until      int64            `json:"until"`
	CurrentRun string           `json:"run"`
	RunsByTask map[string]int   `json:"runs_by_task"`
	Today      store.DayAgg     `json:"today"`
	Items      []store.ItemRow  `json:"items"`
	Totals     store.Totals     `json:"totals"`
	Series     []store.DayPoint `json:"series"`
	LastSeen   int64            `json:"last_seen"`
}

type Dashboard struct {
	Tenant   string     `json:"tenant"`
	Now      int64      `json:"now"`
	Chars    []CharCard `json:"chars"`
	SumToday Sum        `json:"sum_today"`
}

type Sum struct {
	Online   int   `json:"online"`
	Total    int   `json:"total"`
	WorkSec  int64 `json:"work_sec"`
	RunsOK   int   `json:"runs_ok"`
	Picks    int   `json:"picks"`
	Deaths   int   `json:"deaths"`
	Chickens int   `json:"chickens"`
	Errors   int   `json:"errors"`
}

func (h *Handler) Dashboard(w http.ResponseWriter, r *http.Request) {
	t, err := h.st.TenantByViewKey(r.URL.Query().Get("key"))
	if err != nil {
		writeErr(w, http.StatusUnauthorized, "bad view key")
		return
	}
	writeJSON(w, h.dashboard(t))
}

func (h *Handler) dashboard(t *store.Tenant) Dashboard {
	day := time.Now().In(store.CST).Format("2006-01-02")
	d := Dashboard{Tenant: t.Name, Now: time.Now().UnixMilli()}

	rows, err := h.st.DB().Query(
		`SELECT char_name, COALESCE(class,''), COALESCE(machine_id,''), COALESCE(status,''),
		        COALESCE(current_run,''), status_until, ts
		 FROM heartbeats WHERE tenant_id=? ORDER BY char_name`, t.ID)
	if err != nil {
		log.Printf("dashboard query: %v", err)
		return d
	}
	defer rows.Close()

	// 必须先把游标读完再关掉：SQLite 用单连接（SetMaxOpenConns(1)），
	// 若在下层 AggregateDay 里再发查询，会等不到连接而死锁。
	type row struct {
		char, class, machine, status, run string
		until, lastSeen                   int64
	}
	var list []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.char, &r.class, &r.machine, &r.status, &r.run, &r.until, &r.lastSeen); err != nil {
			continue
		}
		list = append(list, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		log.Printf("dashboard rows: %v", err)
	}

	now := time.Now().UnixMilli()
	for _, r := range list {
		var c CharCard
		c.Char, c.Class, c.Machine, c.Status, c.CurrentRun, c.Until = r.char, r.class, r.machine, r.status, r.run, r.until
		lastSeen := r.lastSeen
		c.LastSeen = lastSeen
		c.Online = now-lastSeen < onlineWindow.Milliseconds()
		c.ClassLabel = ClassLabel(c.Class)
		if !c.Online {
			c.Status = "offline"
		}
		c.StatusText = statusText(c)

		agg, err := h.st.AggregateDay(t.ID, c.Char, day)
		if err == nil {
			c.Today = agg
		}
		if m, err := h.st.RunsByTaskToday(t.ID, c.Char, day); err == nil {
			c.RunsByTask = m
		}
		if items, err := h.st.ItemsToday(t.ID, c.Char, day, 200); err == nil {
			c.Items = items
		}
		if tt, err := h.st.TotalsAllTime(t.ID, c.Char); err == nil {
			c.Totals = tt
		}
		if s7, err := h.st.DailySeries(t.ID, c.Char, 7); err == nil {
			c.Series = s7
		}

		d.Chars = append(d.Chars, c)
	}

	d.SumToday.Total = len(d.Chars)
	for _, c := range d.Chars {
		if c.Online {
			d.SumToday.Online++
		}
		d.SumToday.WorkSec += c.Today.WorkSec
		d.SumToday.RunsOK += c.Today.RunsOK
		d.SumToday.Picks += c.Today.Picks
		d.SumToday.Deaths += c.Today.Deaths
		d.SumToday.Chickens += c.Today.Chickens
		d.SumToday.Errors += c.Today.Errors
	}

	sort.Slice(d.Chars, func(i, j int) bool {
		if d.Chars[i].Online != d.Chars[j].Online {
			return d.Chars[i].Online
		}
		return d.Chars[i].Char < d.Chars[j].Char
	})
	return d
}

func statusText(c CharCard) string {
	switch c.Status {
	case "working":
		return "工作中"
	case "resting":
		return "休息中"
	case "blocked":
		return "停机时段"
	case "starting":
		return "启动中"
	case "offline":
		return "离线"
	}
	return c.Status
}

/* ---------------- 工具 ---------------- */

func RandKey() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func decodeJSON(r *http.Request, v any) error {
	defer r.Body.Close()
	return json.NewDecoder(r.Body).Decode(v)
}

func sprintAny(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []byte:
		return string(t)
	case nil:
		return ""
	case int64:
		return itoa(int(t))
	case float64:
		return ftoa(t)
	default:
		b, _ := json.Marshal(t)
		return string(b)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

func ftoa(f float64) string {
	b, _ := json.Marshal(f)
	return string(b)
}
