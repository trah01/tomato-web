package main

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/local/fanqieweb/internal/fqapi"
)

// handlePrefetch 预取指定章节附近的内容。
//
// POST /api/prefetch
//
//	{"bookId":"...","itemId":"..."}          # 预取该章前后 window 章
//	{"bookId":"...","itemId":"...","block":true}  # 阻塞等待当前章就绪
func (s *server) handlePrefetch(w http.ResponseWriter, r *http.Request) {
	if s.prefetch == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "预取服务未启用（未配置正文服务或签名服务）",
		})
		return
	}
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "仅支持 POST"})
		return
	}

	var req struct {
		BookID string `json:"bookId"`
		ItemID string `json:"itemId"`
		Block  bool   `json:"block"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体非法", err)
		return
	}
	if req.BookID == "" {
		req.BookID = s.bookID
	}
	if req.BookID == "" || req.ItemID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "需要 bookId 与 itemId"})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()

	book, err := s.api.WebBookInfo(ctx, req.BookID)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "目录获取失败", err)
		return
	}

	// 定位当前章索引
	idx := -1
	for i, ch := range book.Chapters {
		if ch.ItemID == req.ItemID {
			idx = i
			break
		}
	}
	if idx < 0 {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "章节不在目录中"})
		return
	}

	// 当前章优先入队，再扩散到前后 window 章
	s.prefetch.Request(req.BookID, book.Chapters[idx])
	s.prefetch.PrefetchAround(req.BookID, book.Chapters, idx)

	resp := map[string]any{
		"ok":      true,
		"index":   idx,
		"window":  s.prefetch.Window(),
		"status":  s.prefetch.Status(),
		"itemId":  req.ItemID,
		"bookName": book.Title,
	}

	if req.Block {
		e, werr := s.prefetch.WaitFor(ctx, req.ItemID, 90*time.Second)
		if werr != nil {
			resp["ready"] = false
			resp["error"] = werr.Error()
		} else {
			resp["ready"] = true
			resp["title"] = e.Title
			resp["length"] = e.WordCount
			resp["source"] = e.Source
		}
	}

	writeJSON(w, http.StatusOK, resp)
}

// handlePrefetchStatus 返回预取缓存概况。
func (s *server) handlePrefetchStatus(w http.ResponseWriter, r *http.Request) {
	if s.prefetch == nil {
		writeJSON(w, http.StatusOK, map[string]any{"enabled": false})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled": true,
		"status":  s.prefetch.Status(),
	})
}

// prefetchNext 供前端在翻页时调用（内部使用）。
func (s *server) prefetchAt(bookID string, chapters []fqapi.ChapterMeta, idx int) {
	if s.prefetch == nil || idx < 0 || idx >= len(chapters) {
		return
	}
	s.prefetch.Request(bookID, chapters[idx])
	s.prefetch.PrefetchAround(bookID, chapters, idx)
}
