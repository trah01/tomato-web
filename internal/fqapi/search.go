package fqapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// ContentKey 返回当前内容解密密钥（hex）。
func (c *Client) ContentKey() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.key
}

// SearchResult 搜索结果。
type SearchResult struct {
	BookID    string `json:"bookId"`
	Title     string `json:"title"`
	Author    string `json:"author"`
	Thumb     string `json:"thumb"`
	Abstract  string `json:"abstract"`
	Category  string `json:"category"`
	WordCount int    `json:"wordCount"`
	ReadCount int    `json:"readCount"`
	Status    int    `json:"status"`
	LastTitle string `json:"lastTitle"`
}

// WebSearch 通过网页端搜索接口查书。
//
// 实测端点：
//
//	GET /api/author/search/search_book/v1
//	?filter=127,127,127,127&page_count=10&page_index=0&query_type=0&query_word=X
//
// 该接口需要 msToken + a_bogus 签名；服务端在缺少签名时会返回空响应或非 JSON。
// 因此这里同时携带从首页获取的会话 cookie，并对空响应做明确报错。
func (c *Client) WebSearch(ctx context.Context, keyword string) ([]SearchResult, error) {
	q := url.Values{}
	q.Set("filter", "127,127,127,127")
	q.Set("page_count", "10")
	q.Set("page_index", "0")
	q.Set("query_type", "0")
	q.Set("query_word", keyword)

	u := baseWeb + "/api/author/search/search_book/v1?" + q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36")
	req.Header.Set("Referer", baseWeb+"/")
	req.Header.Set("Accept", "application/json, text/plain, */*")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)
	if len(raw) == 0 {
		return nil, &APIError{Status: resp.StatusCode, Msg: "搜索接口返回空响应（需 msToken/a_bogus 签名，或触发风控）"}
	}

	var payload searchPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, &APIError{Status: resp.StatusCode, Body: string(raw), Msg: "搜索结果解析失败"}
	}
	if payload.Code != 0 {
		return nil, &APIError{Status: resp.StatusCode, Body: string(raw), Msg: "搜索接口返回错误码"}
	}
	return payload.toResults(), nil
}

// ParseSearch 解析搜索接口原始响应，供外部会话复用。
func ParseSearch(raw []byte) ([]SearchResult, error) {
	var payload searchPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, &APIError{Body: string(raw), Msg: "搜索结果解析失败"}
	}
	if payload.Code != 0 {
		return nil, &APIError{Body: string(raw), Msg: "搜索接口返回错误码"}
	}
	return payload.toResults(), nil
}

// toResults 把内部结构转换为对外结果。
func (p searchPayload) toResults() []SearchResult {
	out := make([]SearchResult, 0, len(p.Data.SearchBookDataList))
	for _, b := range p.Data.SearchBookDataList {
		out = append(out, SearchResult{
			BookID:    b.BookID,
			Title:     b.BookName,
			Author:    b.Author,
			Thumb:     firstNonEmptyStr(b.ThumbURL, thumbFromURI(b.ThumbURI)),
			Abstract:  strings.TrimSpace(b.BookAbstract),
			Category:  b.Category,
			WordCount: b.WordCount,
			ReadCount: b.ReadCount,
			Status:    b.CreationStatus,
			LastTitle: b.LastChapterTitle,
		})
	}
	return out
}

type searchPayload struct {
	Code int `json:"code"`
	Data struct {
		SearchAuthorDataList []struct {
			AuthorID   string `json:"author_id"`
			AuthorName string `json:"author_name"`
		} `json:"search_author_data_list"`
		SearchBookDataList []struct {
			Author           string `json:"author"`
			BookAbstract     string `json:"book_abstract"`
			BookID           string `json:"book_id"`
			BookName         string `json:"book_name"`
			Category         string `json:"category"`
			CreationStatus   int    `json:"creation_status"`
			FirstChapterID   string `json:"first_chapter_id"`
			LastChapterID    string `json:"last_chapter_id"`
			LastChapterTime  string `json:"last_chapter_time"`
			LastChapterTitle string `json:"last_chapter_title"`
			ReadCount        int    `json:"read_count"`
			ThumbURI         string `json:"thumb_uri"`
			ThumbURL         string `json:"thumb_url"`
			WordCount        int    `json:"word_count"`
		} `json:"search_book_data_list"`
	} `json:"data"`
}

func thumbFromURI(uri string) string {
	if uri == "" {
		return ""
	}
	return "https://p3-reading-sign.fqnovelpic.com/" + uri + "~tplv-resize:200:260.image"
}

func firstNonEmptyStr(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
