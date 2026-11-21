package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"flag"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/local/fanqieweb/internal/font"
	"github.com/local/fanqieweb/internal/fqapi"
)

//go:embed all:static
var staticFS embed.FS

type server struct {
	api      *fqapi.Client
	web      *fqapi.WebSession
	account  *fqapi.Account
	source   *fqapi.ChapterSource
	prefetch *fqapi.PrefetchCache
	dec      *font.Decoder
	bookID   string
	logger   *log.Logger
}

func main() {
	var (
		addr        = flag.String("addr", envOr("FQ_ADDR", ":8080"), "监听地址")
		bookID      = flag.String("book", "", "默认书籍 ID")
		fontPath    = flag.String("font-map", envOr("FQ_FONT_MAP", ""), "字体映射表 JSON 路径")
		timeout     = flag.Int("timeout", 20, "上游超时（秒）")
		accountFile = flag.String("account-file", envOr("FQ_ACCOUNT_FILE", "data/account.json"), "番茄账号会话存储路径（含登录凭据）")
		pfWindow    = flag.Int("prefetch-window", envOrInt("FQ_PREFETCH_WINDOW", 3), "预取前后章数")
		pfWorkers   = flag.Int("prefetch-workers", envOrInt("FQ_PREFETCH_WORKERS", 1), "预取并发数")
		pfDelayMs   = flag.Int("prefetch-delay-ms", envOrInt("FQ_PREFETCH_DELAY_MS", 1500), "预取每章间隔毫秒")
	)
	flag.Parse()

	logger := log.New(os.Stdout, "[fanqie] ", log.LstdFlags|log.Lmsgprefix)

	// 签名全部在本地纯算完成，不依赖任何外部签名服务。
	api := fqapi.New(fqapi.Options{Timeout: *timeout, Signer: fqapi.NewPureSigner()})

	dec := font.New()
	loaded, err := loadFontMap(dec, *fontPath)
	switch {
	case err == nil:
		logger.Printf("字体映射表已加载: %s (%d 条)", loaded, dec.Size())
	case *fontPath != "":
		logger.Fatalf("字体映射表加载失败: %v", err)
	default:
		logger.Printf("未找到字体映射表，PUA 字符将原样输出（可后续通过 /api/font/reload 加载）")
	}

	if bid := firstNonEmpty(*bookID, os.Getenv("FQ_BOOK_ID")); bid != "" {
		logger.Printf("默认书籍: %s", bid)
	}

	pf := fqapi.NewPrefetchCache(api, dec, fqapi.PrefetchOptions{
		Window:     *pfWindow,
		Workers:    *pfWorkers,
		Delay:      time.Duration(*pfDelayMs) * time.Millisecond,
		MaxEntries: 300,
		Logger:     logger.Printf,
	})
	logger.Printf("预取缓存已启用：前后 %d 章，%d 并发，%dms 间隔", *pfWindow, *pfWorkers, *pfDelayMs)

	srv := &server{
		api:      api,
		source:   fqapi.NewChapterSource(api, dec),
		prefetch: pf,
		web:      fqapi.NewWebSession(time.Duration(*timeout) * time.Second),
		account:  fqapi.NewAccount(*accountFile),
		dec:      dec,
		bookID:   firstNonEmpty(*bookID, os.Getenv("FQ_BOOK_ID")),
		logger:   logger,
	}

	// 恢复上次登录的番茄账号（Cookie 失效时会自动忽略）
	if srv.account != nil {
		loadCtx, loadCancel := context.WithTimeout(context.Background(), 15*time.Second)
		if u := srv.account.Load(loadCtx); u != nil {
			logger.Printf("已恢复番茄账号: %s", u.Name)
		}
		loadCancel()
	}

	if err := srv.web.Warmup(context.Background()); err != nil {
		logger.Printf("网页会话预热失败: %v", err)
	} else {
		logger.Print("网页会话预热完成")
	}

	mux := http.NewServeMux()
	srv.routes(mux)

	httpSrv := &http.Server{
		Addr:              *addr,
		Handler:           withCORS(logReq(mux, logger)),
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		logger.Printf("服务启动: http://localhost%s", *addr)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Fatalf("启动失败: %v", err)
		}
	}()

	<-ctx.Done()
	logger.Print("正在关闭…")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutdownCtx)
}

func (s *server) routes(mux *http.ServeMux) {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		log.Fatalf("静态资源挂载失败: %v", err)
	}
	// 首页
	mux.HandleFunc("/home", s.handleHomePage)
	mux.HandleFunc("/reader", s.handleReaderPage)

	// 静态资源（阅读器页面等）
	fileServer := http.FileServer(http.FS(sub))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// 根路径直接展示首页
		if r.URL.Path == "/" {
			s.handleHomePage(w, r)
			return
		}
		fileServer.ServeHTTP(w, r)
	})

	mux.HandleFunc("/api/health", s.handleHealth)
	mux.HandleFunc("/api/search", s.handleSearch)
	mux.HandleFunc("/api/home", s.handleHome)
	mux.HandleFunc("/api/rank", s.handleRank)
	mux.HandleFunc("/api/rank/peak", s.handlePeakRank)
	mux.HandleFunc("/api/rank/categories", s.handleRank)
	mux.HandleFunc("/api/recent", s.handleRecent)
	mux.HandleFunc("/api/web/attach", s.handleWebAttach)
	mux.HandleFunc("/api/web/status", s.handleWebStatus)

	// 番茄账号：扫码登录、书架同步、阅读进度
	mux.HandleFunc("/api/account", s.handleAccount)
	mux.HandleFunc("/api/account/qr", s.handleQRLogin)
	mux.HandleFunc("/api/account/qr/poll", s.handleQRPoll)
	mux.HandleFunc("/api/account/cookie", s.handleCookieLogin)
	mux.HandleFunc("/api/account/logout", s.handleLogout)
	mux.HandleFunc("/api/account/shelf", s.handleShelf)
	mux.HandleFunc("/api/account/shelf/book", s.handleShelfBook)
	mux.HandleFunc("/api/account/history", s.handleHistory)
	mux.HandleFunc("/api/account/progress", s.handleProgress)
	mux.HandleFunc("/api/book/", s.handleBook)
	mux.HandleFunc("/api/chapter/", s.handleChapter)
	mux.HandleFunc("/api/device", s.handleDevice)
	mux.HandleFunc("/api/font/reload", s.handleFontReload)
	mux.HandleFunc("/api/prefetch", s.handlePrefetch)
	mux.HandleFunc("/api/prefetch/status", s.handlePrefetchStatus)
}

func (s *server) handleHealth(w http.ResponseWriter, r *http.Request) {
	dev := s.api.Device()
	resp := map[string]any{
		"ok":         true,
		"fontMap":    s.dec.Size(),
		"bookId":     s.bookID,
		"deviceId":   "",
		"installId":  "",
		"serverTime": time.Now().Format(time.RFC3339),
	}
	if dev != nil {
		resp["deviceId"] = dev.DeviceID
		resp["installId"] = dev.InstallID
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleDevice 触发设备注册 + 内容密钥申请。
func (s *server) handleDevice(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	if err := s.api.EnsureKey(ctx); err != nil {
		writeErr(w, http.StatusBadGateway, "设备初始化失败", err)
		return
	}
	dev := s.api.Device()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":        true,
		"deviceId":  dev.DeviceID,
		"installId": dev.InstallID,
		"model":     dev.Model,
		"brand":     dev.Brand,
	})
}

// handleBook 返回书籍信息与目录。
// GET /api/book/{bookId}
func (s *server) handleBook(w http.ResponseWriter, r *http.Request) {
	bookID := strings.TrimPrefix(r.URL.Path, "/api/book/")
	bookID = strings.Trim(bookID, "/")
	if bookID == "" {
		bookID = s.bookID
	}
	if bookID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "缺少 bookId"})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()

	book, err := s.api.WebBookInfo(ctx, bookID)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "目录获取失败", err)
		return
	}
	chapters := book.Chapters

	// 目录接口不含书名，从详情页补齐（失败不影响目录返回）
	title, author := book.Title, book.Author
	if title == "" {
		if t, a, e := s.api.FetchBookMeta(ctx, bookID); e == nil {
			title, author = t, a
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"bookId":   bookID,
		"title":    s.decode(title),
		"author":   s.decode(author),
		"chapters": chapters,
		"count":    len(chapters),
	})
}

// handleChapter 返回解密后的章节正文。
// GET /api/chapter/{itemId}?bookId=xxx
func (s *server) handleChapter(w http.ResponseWriter, r *http.Request) {
	itemID := strings.TrimPrefix(r.URL.Path, "/api/chapter/")
	itemID = strings.Trim(itemID, "/")
	if itemID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "缺少 itemId"})
		return
	}
	bookID := r.URL.Query().Get("bookId")
	if bookID == "" {
		bookID = s.bookID
	}

	ctx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
	defer cancel()

	// 首选：预取缓存（毫秒级命中）
	if s.prefetch != nil {
		if e, ok := s.prefetch.Get(itemID); ok {
			writeJSON(w, http.StatusOK, map[string]any{
				"itemId":  itemID,
				"title":   e.Title,
				"content": e.Content,
				"length":  e.WordCount,
				"source":  e.Source + "+cache",
				"cached":  true,
			})
			return
		}
	}

	// 次选：依次尝试正文服务 → App 通道 → 网页通道（见 ChapterSource）
	text, source, err := s.source.Fetch(ctx, bookID, itemID)
	if err != nil {
		s.logger.Printf("取章失败 %s: %v", itemID, err)
		if fqapi.IsRateLimited(err) {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{
				"error":       "章节加载失败",
				"detail":      "番茄签名服务被限流，请稍后重试（连续翻页或批量抓取会触发限制）",
				"rateLimited": "1",
			})
			return
		}
		writeErr(w, http.StatusBadGateway, "章节拉取失败", err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"itemId":  itemID,
		"content": text,
		"length":  len([]rune(text)),
		"hasPUA":  font.HasPUA(text),
		"source":  source,
	})
}

func loadFontMap(dec *font.Decoder, preferred string) (string, error) {
	return dec.AutoLoad(preferred, "data/font_map.json", "/app/data/font_map.json", "/app/defaults/font_map.json")
}

// handleFontReload 热加载字体映射表。
// POST /api/font/reload  body: {"path": "/app/data/font_map.json"}
func (s *server) handleFontReload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "仅支持 POST"})
		return
	}
	var req struct {
		Path string `json:"path"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	var err error
	if req.Path == "" {
		_, err = loadFontMap(s.dec, "")
	} else {
		err = s.dec.Load(req.Path)
	}
	if err != nil {
		writeErr(w, http.StatusBadRequest, "映射表加载失败", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "entries": s.dec.Size()})
}

// handleSearch 优先查询番茄全站，失败时搜索首页书目。
// GET /api/search?q=关键词
func (s *server) handleSearch(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "缺少 q 参数"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()

	// 优先走带签名的网页会话；失败时回退到匿名请求
	results, err := s.searchWithSession(ctx, q)
	if err != nil {
		if fbRes, fbErr := s.api.WebSearch(ctx, q); fbErr == nil {
			results, err = fbRes, nil
		}
	}
	if err != nil {
		homeCtx, homeCancel := context.WithTimeout(r.Context(), 8*time.Second)
		defer homeCancel()
		results, err = s.searchHome(homeCtx, q)
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "站内搜索暂时不可用"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"q": q, "results": results, "count": len(results), "scope": "home"})
		return
	}
	for i := range results {
		results[i].Title = s.decode(results[i].Title)
		results[i].Author = s.decode(results[i].Author)
		results[i].Abstract = s.decode(results[i].Abstract)
	}
	writeJSON(w, http.StatusOK, map[string]any{"q": q, "results": results, "count": len(results)})
}

// ---- 工具 ----

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string, err error) {
	body := map[string]string{"error": msg}
	if err != nil {
		body["detail"] = err.Error()
	}
	writeJSON(w, status, body)
}

// envOrInt 返回整数环境变量值。
func envOrInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

// envOr 返回环境变量值，为空时用默认值。
func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// withCORS 处理跨域预检。
//
// 不设置 Access-Control-Allow-Origin：服务端持有登录会话，
// 若放开任意来源，任何网页都能借用户浏览器读取书架、改写阅读进度。
// 页面与接口同源加载，不需要跨域。
func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func logReq(next http.Handler, logger *log.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		logger.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(start).Round(time.Millisecond))
	})
}
