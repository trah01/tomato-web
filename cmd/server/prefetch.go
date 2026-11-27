package main

import (
	"context"
	"encoding/json"
	"net/http"
	"time"
)

// handlePrefetch 预取接下来要读的章节。
//
// POST /api/prefetch
//
//	{"bookId":"...","itemIds":["a","b","c"]}     # 合并成一次批量请求（推荐）
//	{"bookId":"...","itemId":"..."}              # 单章 + 前后 window 章
//	{"bookId":"...","itemId":"...","block":true} # 阻塞等待该章就绪
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
		BookID  string   `json:"bookId"`
		ItemID  string   `json:"itemId"`
		ItemIDs []string `json:"itemIds"`
		Block   bool     `json:"block"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体非法", err)
		return
	}
	if req.BookID == "" {
		req.BookID = s.bookID
	}
	if len(req.ItemIDs) > 0 {
		// 前端已知道要读哪几章：直接按 id 批量入队，不用再拉一次目录。
		if req.BookID == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "需要 bookId"})
			return
		}
		queued := s.prefetch.WarmItemIDs(req.BookID, req.ItemIDs)
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":     true,
			"queued": queued,
			"status": s.prefetch.Status(),
		})
		return
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
		"ok":       true,
		"index":    idx,
		"window":   s.prefetch.Window(),
		"status":   s.prefetch.Status(),
		"itemId":   req.ItemID,
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
