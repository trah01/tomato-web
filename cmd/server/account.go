package main

import (
	"context"
	"encoding/json"
	"mime"
	"net/http"
	"strconv"
	"time"

	"github.com/local/fanqieweb/internal/fqapi"
)

// 番茄账号相关接口：扫码登录、书架同步、阅读进度读写。

// handleAccount 返回账号状态。
// GET /api/account
func (s *server) handleAccount(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, s.account.Status())
}

// handleQRLogin 获取登录二维码。
// POST /api/account/qr
func (s *server) handleQRLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "仅支持 POST"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()

	qr, err := s.account.StartQRLogin(ctx)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "获取二维码失败", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"token":    qr.Token,
		"image":    qr.Image,
		"text":     qr.Text,
		"expireAt": qr.Expire,
	})
}

// handleQRPoll 轮询扫码状态，确认后自动建立会话。
// GET /api/account/qr/poll?token=xxx
func (s *server) handleQRPoll(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("token")
	if token == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "缺少 token"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()

	st, err := s.account.PollQRLogin(ctx, token)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "查询扫码状态失败", err)
		return
	}

	// 已确认：跟随跳转建立会话
	if st.State == "confirmed" {
		user, ferr := s.account.FinishQRLogin(ctx, st.Redirected)
		if ferr != nil {
			writeJSON(w, http.StatusOK, map[string]any{
				"state":   "error",
				"message": "扫码已确认，但未能建立会话，请重试或改用粘贴 Cookie",
				"detail":  ferr.Error(),
			})
			return
		}
		s.logger.Printf("番茄账号已登录: %s", user.Name)
		writeJSON(w, http.StatusOK, map[string]any{
			"state":   "success",
			"message": "登录成功：" + user.Name,
			"user":    user,
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"state":   st.State,
		"message": st.Message,
	})
}

// handleCookieLogin 用浏览器复制的 Cookie 登录。
// POST /api/account/cookie  {"cookie": "sessionid=...; ..."}
func (s *server) handleCookieLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "仅支持 POST"})
		return
	}
	var req struct {
		Cookie string `json:"cookie"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON", err)
		return
	}
	if req.Cookie == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "缺少 cookie"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()

	user, err := s.account.LoginWithCookie(ctx, req.Cookie)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "Cookie 登录失败", err)
		return
	}
	s.logger.Printf("番茄账号已登录: %s", user.Name)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "user": user})
}

// handleLogout 退出登录。
// POST /api/account/logout
func (s *server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "仅支持 POST"})
		return
	}
	if err := s.account.Logout(); err != nil {
		writeErr(w, http.StatusInternalServerError, "退出登录失败", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleShelf 返回云端书架。
// GET /api/account/shelf
func (s *server) handleShelf(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	books, err := s.account.VShelf(ctx)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error":    "获取书架失败",
			"detail":   err.Error(),
			"loggedIn": s.account.LoggedIn(),
		})
		return
	}
	s.decodeShelfBooks(books)
	writeJSON(w, http.StatusOK, map[string]any{"count": len(books), "books": books})
}

func (s *server) handleShelfBook(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		w.Header().Set("Allow", "GET, POST")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "仅支持 GET 和 POST"})
		return
	}
	if !s.account.LoggedIn() {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "请先登录"})
		return
	}
	bookID := r.URL.Query().Get("bookId")
	if r.Method == http.MethodPost {
		mediaType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if mediaType != "application/json" {
			writeJSON(w, http.StatusUnsupportedMediaType, map[string]string{"error": "需要 JSON 请求"})
			return
		}
		var req struct {
			BookID string `json:"bookId"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "书籍请求无效"})
			return
		}
		bookID = req.BookID
	}
	if id, err := strconv.ParseUint(bookID, 10, 64); err != nil || id == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "书籍 ID 无效"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	if r.Method == http.MethodPost {
		if err := s.account.AddShelfBook(ctx, bookID); err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "加入书架失败，请重试"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"added": true})
		return
	}
	added, err := s.account.HasShelfBook(ctx, bookID)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "无法查看书架状态"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"added": added})
}

// handleHistory 返回云端阅读历史。
func (s *server) handleHistory(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !s.account.LoggedIn() {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "请先登录"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()
	books, err := s.account.ReadHistory(ctx)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "获取阅读历史失败"})
		return
	}
	s.decodeShelfBooks(books)
	writeJSON(w, http.StatusOK, map[string]any{"count": len(books), "books": books})
}

func (s *server) decodeShelfBooks(books []fqapi.ShelfBook) {
	for i := range books {
		books[i].Title = s.decode(books[i].Title)
		books[i].Author = s.decode(books[i].Author)
		books[i].ChapterTitle = s.decode(books[i].ChapterTitle)
	}
}

// handleProgress 读取或上报阅读进度。
// GET  /api/account/progress            读取云端进度
// POST /api/account/progress            上报进度 {"bookId":"..","itemId":"..","order":0}
func (s *server) handleProgress(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()

	if r.Method == http.MethodPost {
		var req struct {
			BookID string `json:"bookId"`
			ItemID string `json:"itemId"`
			Order  int    `json:"order"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON", err)
			return
		}
		if err := s.account.UpdateProgress(ctx, req.BookID, req.ItemID, req.Order); err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]any{
				"error":  "进度上报失败",
				"detail": err.Error(),
			})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}

	list, err := s.account.ReadProgress(ctx)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error":    "读取进度失败",
			"detail":   err.Error(),
			"loggedIn": s.account.LoggedIn(),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"count": len(list), "progress": list})
}
