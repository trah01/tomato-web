package main

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/local/fanqieweb/internal/fqapi"
)

func (s *server) searchHome(ctx context.Context, query string) ([]fqapi.SearchResult, error) {
	data, err := s.api.FetchHome(ctx)
	if err != nil {
		return nil, err
	}
	query = strings.ToLower(query)
	results := make([]fqapi.SearchResult, 0)
	seen := make(map[string]bool)
	add := func(book fqapi.SearchResult) {
		if book.BookID == "" || seen[book.BookID] {
			return
		}
		book.Title = s.decode(book.Title)
		book.Author = s.decode(book.Author)
		if !strings.Contains(strings.ToLower(book.Title), query) &&
			!strings.Contains(strings.ToLower(book.Author), query) {
			return
		}
		book.Abstract = s.decode(book.Abstract)
		results = append(results, book)
		seen[book.BookID] = true
	}
	for _, b := range data.Recommends {
		add(fqapi.SearchResult{BookID: b.BookID, Title: b.BookName, Author: b.Author, Thumb: b.Thumb, Abstract: b.Abstract, Category: b.Category})
	}
	for _, b := range data.RankTop {
		add(fqapi.SearchResult{BookID: b.BookID, Title: b.BookName, Author: b.Author, Thumb: b.Thumb, Abstract: b.Abstract, Category: b.Category})
	}
	for _, b := range data.Recents {
		add(fqapi.SearchResult{BookID: b.BookID, Title: b.BookName, Author: b.Author, Category: b.Category})
	}
	return results, nil
}

// handleHome 首页聚合数据（推荐 + 榜单 + 最近更新）。
// GET /api/home
func (s *server) handleHome(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()

	data, err := s.api.FetchHome(ctx)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error":  "首页数据获取失败",
			"detail": err.Error(),
		})
		return
	}

	s.decodeRank(data.RankTop)
	for i := range data.Recommends {
		data.Recommends[i].BookName = s.decode(data.Recommends[i].BookName)
		data.Recommends[i].Author = s.decode(data.Recommends[i].Author)
		data.Recommends[i].Abstract = s.decode(data.Recommends[i].Abstract)
	}
	for i := range data.Recents {
		data.Recents[i].BookName = s.decode(data.Recents[i].BookName)
		data.Recents[i].Chapter = s.decode(data.Recents[i].Chapter)
		data.Recents[i].Author = s.decode(data.Recents[i].Author)
	}

	// 同步调整简介长度，避免前端过长
	for i := range data.Recommends {
		data.Recommends[i].Abstract = fqapi.TrimAbstract(data.Recommends[i].Abstract, 110)
	}
	for i := range data.RankTop {
		data.RankTop[i].Abstract = fqapi.TrimAbstract(data.RankTop[i].Abstract, 90)
	}
	writeJSON(w, http.StatusOK, data)
}

func (s *server) handlePeakRank(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	books, err := s.api.FetchPeakRank(ctx)
	if err != nil {
		s.logger.Printf("巅峰榜获取失败: %v", err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "巅峰榜暂时无法加载"})
		return
	}
	for i := range books {
		books[i].BookName = s.decode(books[i].BookName)
		books[i].Category = s.decode(books[i].Category)
	}
	writeJSON(w, http.StatusOK, map[string]any{"books": books})
}

// handleRank 榜单分类列表 + 榜单内容。
// GET /api/rank/categories
// GET /api/rank?gender=1&categoryId=1141&mold=2&offset=0&limit=20
func (s *server) handleRank(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/api/rank/categories" {
		writeJSON(w, http.StatusOK, map[string]any{
			"male":   fqapi.RankCategories[1],
			"female": fqapi.RankCategories[2],
		})
		return
	}

	q := r.URL.Query()
	gender := atoiDefault(q.Get("gender"), 1)
	catID := atoiDefault(q.Get("categoryId"), 1141)
	mold := atoiDefault(q.Get("mold"), 2)
	offset := atoiDefault(q.Get("offset"), 0)
	limit := atoiDefault(q.Get("limit"), 20)
	if limit > 50 {
		limit = 50
	}

	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()

	books, total, err := s.api.FetchRank(ctx, gender, catID, mold, offset, limit)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error":  "榜单获取失败",
			"detail": err.Error(),
		})
		return
	}
	s.decodeRank(books)
	for i := range books {
		books[i].Abstract = fqapi.TrimAbstract(books[i].Abstract, 100)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"gender":     gender,
		"categoryId": catID,
		"category":   fqapi.CategoryName(gender, catID),
		"mold":       mold,
		"moldName":   map[int]string{1: "新书榜", 2: "阅读榜"}[mold],
		"offset":     offset,
		"limit":      limit,
		"total":      total,
		"books":      books,
	})
}

// handleRecent 最近更新。
// GET /api/recent?limit=20
func (s *server) handleRecent(w http.ResponseWriter, r *http.Request) {
	limit := atoiDefault(r.URL.Query().Get("limit"), 20)
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	list, err := s.api.FetchRecentUpdate(ctx, limit)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error": "更新列表获取失败", "detail": err.Error(),
		})
		return
	}
	for i := range list {
		list[i].BookName = s.decode(list[i].BookName)
		list[i].Chapter = s.decode(list[i].Chapter)
		list[i].Author = s.decode(list[i].Author)
	}
	writeJSON(w, http.StatusOK, map[string]any{"list": list, "count": len(list)})
}

// decode 用字体映射还原 PUA 混淆字符。
func (s *server) decode(v string) string {
	if s.dec == nil || v == "" {
		return v
	}
	return s.dec.Decode(v)
}

// decodeRank 批量还原榜单里的书名/作者/简介。
func (s *server) decodeRank(books []fqapi.RankBook) {
	for i := range books {
		books[i].BookName = s.decode(books[i].BookName)
		books[i].Author = s.decode(books[i].Author)
		books[i].Abstract = s.decode(books[i].Abstract)
		books[i].LastChapter = s.decode(books[i].LastChapter)
	}
}

func atoiDefault(s string, def int) int {
	if s == "" {
		return def
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return v
}
