package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/local/fanqieweb/internal/fqapi"
)

// handleWebAttach 注入浏览器侧生成的 msToken / a_bogus / cookie。
//
// 用法：在 fanqienovel.com 页面控制台执行一次抓取脚本，把结果 POST 过来：
//
//	POST /api/web/attach
//	{"msToken":"...","a_bogus":"...","cookie":"..."}
func (s *server) handleWebAttach(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "仅支持 POST"})
		return
	}
	var req struct {
		MsToken string `json:"msToken"`
		ABogus  string `json:"a_bogus"`
		Cookie  string `json:"cookie"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON", err)
		return
	}
	if req.MsToken == "" && req.ABogus == "" && req.Cookie == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "至少提供 msToken / a_bogus / cookie 之一"})
		return
	}
	s.web.Attach(req.MsToken, req.ABogus, req.Cookie)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "status": s.web.Status()})
}

// handleWebStatus 返回网页会话状态。
func (s *server) handleWebStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.web.Status())
}

// handleSearchSession 用网页会话执行搜索（需要已 attach 签名材料）。
func (s *server) searchWithSession(ctx context.Context, keyword string) ([]fqapi.SearchResult, error) {
	q := make(map[string][]string)
	_ = q
	vals := urlValues(keyword)
	raw, err := s.web.Get(ctx, "/api/author/search/search_book/v1", vals)
	if err != nil {
		return nil, err
	}
	return fqapi.ParseSearch(raw)
}

// urlValues 构造搜索参数。
func urlValues(keyword string) map[string][]string {
	return map[string][]string{
		"filter":     {"127,127,127,127"},
		"page_count": {"10"},
		"page_index": {"0"},
		"query_type": {"0"},
		"query_word": {keyword},
	}
}

var _ = strings.TrimSpace
var _ = time.Second
