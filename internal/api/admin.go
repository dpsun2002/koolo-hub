package api

import (
	"net/http"
	"strings"

	"koolo-hub/internal/store"
)

// AdminToken 由 serve 启动时注入；为空则 admin 接口全部禁用。
type AdminHandler struct {
	st         *store.Store
	adminToken string
	allowWrite bool
}

func NewAdmin(st *store.Store, adminToken string, allowWrite bool) *AdminHandler {
	return &AdminHandler{st: st, adminToken: adminToken, allowWrite: allowWrite}
}

func (a *AdminHandler) authorized(r *http.Request) bool {
	if a.adminToken == "" {
		return false
	}
	t := r.Header.Get("X-Admin-Token")
	if t == "" {
		t = r.URL.Query().Get("token")
	}
	return t == a.adminToken
}

// Tables 列出所有业务表、行数、建表语句
func (a *AdminHandler) Tables(w http.ResponseWriter, r *http.Request) {
	if !a.authorized(r) {
		writeErr(w, http.StatusUnauthorized, "admin auth required")
		return
	}
	ts, err := a.st.ListTables()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]any{"tables": ts})
}

// Query 执行 SQL；默认只读（SELECT/WITH/EXPLAIN）。--allow-write 时开放 DML。
func (a *AdminHandler) Query(w http.ResponseWriter, r *http.Request) {
	if !a.authorized(r) {
		writeErr(w, http.StatusUnauthorized, "admin auth required")
		return
	}

	sqlText := strings.TrimSpace(r.URL.Query().Get("sql"))
	limit := atoiDefault(r.URL.Query().Get("limit"), 200)
	if r.Method == http.MethodPost {
		var body struct {
			SQL   string `json:"sql"`
			Limit int    `json:"limit"`
		}
		_ = decodeJSON(r, &body)
		if strings.TrimSpace(body.SQL) != "" {
			sqlText = strings.TrimSpace(body.SQL)
		}
		if body.Limit > 0 {
			limit = body.Limit
		}
	}
	if sqlText == "" {
		writeErr(w, http.StatusBadRequest, "empty sql")
		return
	}
	if !a.safeSQL(sqlText) {
		writeErr(w, http.StatusForbidden, "非只读语句被拒绝；如需写操作，用 --allow-write 启动服务端")
		return
	}

	res, err := a.st.Query(sqlText, limit)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	if r.URL.Query().Get("format") == "csv" {
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		w.Header().Set("Content-Disposition", "attachment; filename=export.csv")
		writeCSV(w, res)
		return
	}
	writeJSON(w, res)
}

// safeSQL 仅允许单条只读语句，杜绝堆叠注入。
func (a *AdminHandler) safeSQL(sqlText string) bool {
	upper := strings.ToUpper(strings.TrimSpace(sqlText))
	if strings.Contains(sqlText, ";") || strings.Contains(sqlText, "\n--") {
		return false // 禁止多语句
	}
	if a.allowWrite {
		return true
	}
	for _, p := range []string{"SELECT ", "SELECT\t", "WITH ", "EXPLAIN "} {
		if strings.HasPrefix(upper, p) {
			return true
		}
	}
	return false
}

func atoiDefault(s string, def int) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return def
		}
		n = n*10 + int(c-'0')
	}
	if n == 0 {
		return def
	}
	return n
}

func writeCSV(w http.ResponseWriter, res store.QueryResult) {
	esc := func(v any) string {
		s := ""
		switch t := v.(type) {
		case nil:
			s = ""
		case string:
			s = t
		case []byte:
			s = string(t)
		default:
			s = sprintAny(v)
		}
		if strings.Contains(s, ",") || strings.Contains(s, "\"") || strings.Contains(s, "\n") {
			return "\"" + strings.ReplaceAll(s, "\"", "\"\"") + "\""
		}
		return s
	}
	for i, c := range res.Columns {
		if i > 0 {
			_, _ = w.Write([]byte(","))
		}
		_, _ = w.Write([]byte(esc(c)))
	}
	_, _ = w.Write([]byte("\n"))
	for _, row := range res.Rows {
		for i, v := range row {
			if i > 0 {
				_, _ = w.Write([]byte(","))
			}
			_, _ = w.Write([]byte(esc(v)))
		}
		_, _ = w.Write([]byte("\n"))
	}
}
