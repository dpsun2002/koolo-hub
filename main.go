package main

import (
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"time"

	"koolo-hub/internal/api"
	"koolo-hub/internal/store"
)

//go:embed internal/web
var webFS embed.FS

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)

	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	switch os.Args[1] {
	case "serve":
		cmdServe(os.Args[2:])
	case "tenant":
		cmdTenant(os.Args[2:])
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `koolo-hub —— koolo 多机集中监控服务端

用法:
  koolo-hub serve   --addr :8080 --db /var/lib/koolo-hub/hub.db
  koolo-hub tenant add  --name "张三"
  koolo-hub tenant list [--db ...]
`)
}

func cmdServe(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", ":8080", "监听地址")
	dbPath := fs.String("db", "./hub.db", "SQLite 数据库路径")
	purgeDays := fs.Int("purge-days", 90, "事件/会话明细保留天数（daily 聚合永久保留）")
	adminToken := fs.String("admin-token", os.Getenv("KOLOO_ADMIN_TOKEN"), "数据库查询台密钥；为空则自动生成并打印在日志中")
	allowWrite := fs.Bool("allow-write", false, "允许 /admin 执行 INSERT/UPDATE/DELETE 等写操作（默认只读）")
	fs.Parse(args)

	st, err := store.Open(*dbPath)
	if err != nil {
		log.Fatalf("打开数据库失败: %v", err)
	}
	defer st.Close()

	if *adminToken == "" {
		*adminToken = api.RandKey()
		log.Printf("ADMIN TOKEN (请妥善保存，用于 /admin 数据库查询台): %s", *adminToken)
	} else {
		log.Printf("ADMIN TOKEN 已通过参数/环境变量注入（长度 %d）", len(*adminToken))
	}

	go materializer(st)
	go purger(st, *purgeDays)

	h := api.New(st)
	adm := api.NewAdmin(st, *adminToken, *allowWrite)
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	})
	mux.HandleFunc("/api/v1/ingest", h.Ingest)
	mux.HandleFunc("/api/v1/dashboard", h.Dashboard)
	mux.HandleFunc("/api/v1/admin/tables", adm.Tables)
	mux.HandleFunc("/api/v1/admin/query", adm.Query)
	mux.HandleFunc("/admin", func(w http.ResponseWriter, r *http.Request) {
		data, err := webFS.ReadFile("internal/web/admin.html")
		if err != nil {
			http.Error(w, "admin console not embedded", 500)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(data)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		data, err := webFS.ReadFile("internal/web/index.html")
		if err != nil {
			http.Error(w, "dashboard not embedded", 500)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(data)
	})

	// 上报接口限流 + body 限制，防止客户端误配死循环打爆服务
	handler := limitBody(8<<20, mux)

	log.Printf("koolo-hub 监听 %s (db=%s)", *addr, *dbPath)
	srv := &http.Server{
		Addr:              *addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      30 * time.Second,
	}
	if err := srv.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}

func cmdTenant(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "需要子命令: add | list")
		os.Exit(2)
	}
	fs := flag.NewFlagSet("tenant", flag.ExitOnError)
	dbPath := fs.String("db", "./hub.db", "SQLite 数据库路径")
	name := fs.String("name", "", "租户名称")
	fs.Parse(args[1:])

	st, err := store.Open(*dbPath)
	if err != nil {
		log.Fatalf("打开数据库失败: %v", err)
	}
	defer st.Close()

	switch args[0] {
	case "add":
		if *name == "" {
			log.Fatal("需要 --name")
		}
		t := store.Tenant{
			ID:        api.RandKey(),
			Name:      *name,
			IngestKey: api.RandKey(),
			ViewKey:   api.RandKey(),
			CreatedAt: time.Now().UnixMilli(),
		}
		if err := st.CreateTenant(t); err != nil {
			log.Fatalf("创建租户失败: %v", err)
		}
		fmt.Println("租户创建成功，请把下面两行发给对方（ingest_key 是写入密钥，勿泄露给第三方）：")
		fmt.Printf("  名称       : %s\n", t.Name)
		fmt.Printf("  ingest_key : %s   ← 填进客户端 config.yaml 的 telemetry.key\n", t.IngestKey)
		fmt.Printf("  view_key   : %s   ← 看板查看密钥\n", t.ViewKey)
		fmt.Printf("  看板地址   : http://<服务器地址>/?key=%s\n", t.ViewKey)

	case "list":
		ts, err := st.ListTenants()
		if err != nil {
			log.Fatal(err)
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(ts)

	default:
		fmt.Fprintln(os.Stderr, "未知子命令")
		os.Exit(2)
	}
}

// materializer 定期重算 daily 物化表（今日+近 7 日），看板趋势图读的就是它。
func materializer(st *store.Store) {
	run := func() {
		chars, err := st.ActiveCharacters(time.Now().AddDate(0, 0, -7).UnixMilli())
		if err != nil {
			log.Printf("materialize: 查询角色失败 %v", err)
			return
		}
		for _, c := range chars {
			if err := st.Materialize(c.TenantID, c.Char, 7); err != nil {
				log.Printf("materialize %s/%s: %v", c.TenantID, c.Char, err)
			}
		}
	}
	run() // 启动时先跑一次，避免首日历史累计为空
	t := time.NewTicker(3 * time.Minute)
	defer t.Stop()
	for range t.C {
		run()
	}
}

func purger(st *store.Store, days int) {
	t := time.NewTicker(12 * time.Hour)
	defer t.Stop()
	for range t.C {
		if err := st.Purge(days); err != nil {
			log.Printf("purge: %v", err)
		}
	}
}

func limitBody(n int64, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, n)
		}
		next.ServeHTTP(w, r)
	})
}

var _ = io.Discard
