package fqapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---- 首页数据：推荐 / 榜单 / 最近更新（全部免签） ----

// Category 是榜单分类。
type Category struct {
	ID     int    `json:"id"`
	Name   string `json:"name"`
	Gender int    `json:"gender"` // 1=男频 2=女频
}

// RankCategories 男频与女频的榜单分类（页面链接中的真实 ID）。
var RankCategories = map[int][]Category{
	1: {
		{1141, "西方奇幻", 1}, {1140, "东方仙侠", 1}, {8, "科幻末世", 1},
		{261, "都市日常", 1}, {124, "都市修真", 1}, {1014, "都市高武", 1},
		{273, "历史古代", 1}, {27, "战神赘婿", 1}, {263, "都市种田", 1},
		{258, "传统玄幻", 1}, {272, "历史脑洞", 1}, {539, "悬疑脑洞", 1},
		{262, "都市脑洞", 1}, {257, "玄幻脑洞", 1}, {751, "悬疑灵异", 1},
		{504, "抗战谍战", 1}, {746, "游戏体育", 1}, {718, "动漫衍生", 1},
		{1016, "男频衍生", 1},
	},
	2: {
		{1139, "古风世情", 2}, {8, "科幻末世", 2}, {746, "游戏体育", 2},
		{1015, "女频衍生", 2}, {248, "玄幻言情", 2}, {23, "种田", 2},
		{79, "年代", 2}, {267, "现言脑洞", 2}, {246, "宫斗宅斗", 2},
		{539, "悬疑脑洞", 2}, {253, "古言脑洞", 2}, {24, "快穿", 2},
		{749, "青春甜宠", 2}, {745, "星光璀璨", 2}, {747, "女频悬疑", 2},
		{750, "职场婚恋", 2}, {748, "豪门总裁", 2}, {1017, "民国言情", 2},
	},
}

// RankBook 是榜单里的一本书。
type RankBook struct {
	BookID         string `json:"bookId"`
	BookName       string `json:"bookName"`
	Author         string `json:"author"`
	Abstract       string `json:"abstract"`
	Category       string `json:"category"`
	Thumb          string `json:"thumb"`
	ReadCount      int64  `json:"readCount"`
	WordNumber     int64  `json:"wordNumber"`
	CurrentPos     int    `json:"pos"`
	RankPosDiff    int    `json:"posDiff"`
	LastChapter    string `json:"lastChapter"`
	CreationStatus int    `json:"status"`
	FirstChapter   string `json:"firstChapterItemId"`
}

// RecommendBook 是首页推荐位的一本书。
type RecommendBook struct {
	BookID   string `json:"bookId"`
	BookName string `json:"bookName"`
	Author   string `json:"author"`
	Abstract string `json:"abstract"`
	Category string `json:"category"`
	Thumb    string `json:"thumb"`
}

// PeakBook 是番茄巅峰榜里的一本书，顺序由上游决定。
type PeakBook struct {
	BookID   string `json:"bookId"`
	BookName string `json:"bookName"`
	Category string `json:"category"`
	Thumb    string `json:"thumb"`
}

// RecentUpdate 是最近更新的一章。
type RecentUpdate struct {
	BookID     string `json:"bookId"`
	BookName   string `json:"bookName"`
	ItemID     string `json:"itemId"`
	Chapter    string `json:"chapter"`
	Category   string `json:"category"`
	Author     string `json:"author"`
	UpdateTime int64  `json:"updateTime"`
}

const homeRefreshInterval = time.Hour

type homeResponse struct {
	ready chan struct{}
	mu    sync.RWMutex
	raw   []byte
	err   error
}

func (entry *homeResponse) refresh(fetch func() ([]byte, error)) {
	raw, err := fetch()
	entry.mu.Lock()
	if err == nil || entry.raw == nil {
		entry.raw, entry.err = raw, err
	}
	entry.mu.Unlock()
}

func webGetJSON[T any](c *Client, ctx context.Context, path string, q url.Values, cache bool, v *T) error {
	u := baseWeb + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	if !cache {
		raw, err := c.fetchWebJSON(ctx, u)
		if err != nil {
			return err
		}
		return json.Unmarshal(raw, v)
	}

	value, exists := c.homeResponses.LoadOrStore(u, &homeResponse{ready: make(chan struct{})})
	entry := value.(*homeResponse)
	if !exists {
		go func() {
			fetch := func() ([]byte, error) {
				raw, err := c.fetchWebJSON(context.Background(), u)
				if err != nil {
					return nil, err
				}
				var parsed T
				if err := json.Unmarshal(raw, &parsed); err != nil {
					return nil, err
				}
				return raw, nil
			}
			entry.refresh(fetch)
			close(entry.ready)
			ticker := time.NewTicker(homeRefreshInterval)
			defer ticker.Stop()
			for range ticker.C {
				entry.refresh(fetch)
			}
		}()
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-entry.ready:
	}
	entry.mu.RLock()
	raw, err := entry.raw, entry.err
	entry.mu.RUnlock()
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, v)
}

func (c *Client) fetchWebJSON(ctx context.Context, u string) ([]byte, error) {
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
	if resp.StatusCode != http.StatusOK {
		return nil, &APIError{Status: resp.StatusCode, Msg: "首页接口返回异常状态"}
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var envelope struct {
		Code *int `json:"code"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil || envelope.Code == nil || *envelope.Code != 0 {
		return nil, &APIError{Status: resp.StatusCode, Msg: "首页接口返回异常数据"}
	}
	return raw, nil
}

// FetchRecommend 首页推荐位。
func (c *Client) FetchRecommend(ctx context.Context) ([]RecommendBook, error) {
	var jr struct {
		Data struct {
			List []struct {
				BookID   string `json:"bookId"`
				ThumbURI string `json:"thumbUri"`
				BookName string `json:"bookName"`
				Abstract string `json:"abstract"`
				Author   string `json:"author"`
				Category string `json:"category"`
			} `json:"list"`
		} `json:"data"`
	}
	if err := webGetJSON(c, ctx, "/api/rank/recommend/list", nil, true, &jr); err != nil {
		return nil, err
	}
	out := make([]RecommendBook, 0, len(jr.Data.List))
	for _, b := range jr.Data.List {
		out = append(out, RecommendBook{
			BookID: b.BookID, BookName: b.BookName, Author: b.Author,
			Abstract: b.Abstract, Category: b.Category, Thumb: b.ThumbURI,
		})
	}
	return out, nil
}

// FetchPeakRank 获取番茄官网首页的巅峰榜。
func (c *Client) FetchPeakRank(ctx context.Context) ([]PeakBook, error) {
	q := url.Values{"limit": {"200"}, "offset": {"0"}}
	var payload struct {
		BookList []struct {
			BookID   string `json:"book_id"`
			BookName string `json:"book_name"`
			Category string `json:"category"`
			ThumbURL string `json:"thumb_url"`
		} `json:"book_list"`
	}
	if err := webGetJSON(c, ctx, "/api/author/misc/top_book_list/v1/", q, true, &payload); err != nil {
		return nil, err
	}
	books := make([]PeakBook, 0, len(payload.BookList))
	for _, book := range payload.BookList {
		if book.BookID != "" && book.BookName != "" {
			books = append(books, PeakBook{BookID: book.BookID, BookName: book.BookName, Category: book.Category, Thumb: book.ThumbURL})
		}
	}
	return books, nil
}

// FetchRank 拉取某个榜单分类，rankMold 1=新书榜 2=阅读榜。
func (c *Client) FetchRank(ctx context.Context, gender, categoryID, rankMold, offset, limit int) ([]RankBook, int, error) {
	q := url.Values{}
	q.Set("app_id", "2503")
	q.Set("rank_list_type", "3")
	q.Set("offset", strconv.Itoa(offset))
	q.Set("limit", strconv.Itoa(limit))
	q.Set("category_id", strconv.Itoa(categoryID))
	q.Set("rank_version", "")
	q.Set("gender", strconv.Itoa(gender))
	q.Set("rankMold", strconv.Itoa(rankMold))

	var jr struct {
		Data struct {
			RankTypeText string      `json:"rankTypeText"`
			TotalNum     json.Number `json:"total_num"`
			BookList     []struct {
				BookID           string      `json:"bookId"`
				BookName         string      `json:"bookName"`
				Author           string      `json:"author"`
				Abstract         string      `json:"abstract"`
				Category         string      `json:"category"`
				ThumbURI         string      `json:"thumbUri"`
				ReadCount        json.Number `json:"readCount"`
				ReadCountAlt     json.Number `json:"read_count"`
				WordNumber       json.Number `json:"wordNumber"`
				CurrentPos       int         `json:"currentPos"`
				RankPosDiff      int         `json:"rankPosDiff"`
				LastChapterTitle string      `json:"lastChapterTitle"`
				CreationStatus   json.Number `json:"creationStatus"`
				FirstChapter     string      `json:"firstChapterItemId"`
			} `json:"book_list"`
		} `json:"data"`
	}
	cache := offset == 0 && limit == 12 && rankMold == 2 && hasRankCategory(gender, categoryID)
	if err := webGetJSON(c, ctx, "/api/rank/category/list", q, cache, &jr); err != nil {
		return nil, 0, err
	}

	out := make([]RankBook, 0, len(jr.Data.BookList))
	for _, b := range jr.Data.BookList {
		rc := numToInt64(b.ReadCount)
		if rc == 0 {
			rc = numToInt64(b.ReadCountAlt)
		}
		out = append(out, RankBook{
			BookID: b.BookID, BookName: b.BookName, Author: b.Author,
			Abstract: b.Abstract, Category: b.Category, Thumb: b.ThumbURI,
			ReadCount: rc, WordNumber: numToInt64(b.WordNumber),
			CurrentPos: b.CurrentPos, RankPosDiff: b.RankPosDiff,
			LastChapter: b.LastChapterTitle, CreationStatus: int(numToInt64(b.CreationStatus)),
			FirstChapter: b.FirstChapter,
		})
	}
	return out, int(numToInt64(jr.Data.TotalNum)), nil
}

// FetchRecentUpdate 最近更新列表。
func (c *Client) FetchRecentUpdate(ctx context.Context, limit int) ([]RecentUpdate, error) {
	if limit <= 0 {
		limit = 20
	}
	var jr struct {
		Data struct {
			Data []struct {
				BookName   string `json:"bookName"`
				ItemID     string `json:"itemId"`
				BookID     string `json:"bookId"`
				Title      string `json:"title"`
				Category   string `json:"category"`
				UpdateTime string `json:"updateTime"`
				Author     string `json:"author"`
			} `json:"data"`
		} `json:"data"`
	}
	if err := webGetJSON(c, ctx, "/api/rank/recent/update/list", nil, true, &jr); err != nil {
		return nil, err
	}
	out := make([]RecentUpdate, 0, len(jr.Data.Data))
	for _, r := range jr.Data.Data {
		ts, _ := strconv.ParseInt(r.UpdateTime, 10, 64)
		out = append(out, RecentUpdate{
			BookID: r.BookID, BookName: r.BookName, ItemID: r.ItemID,
			Chapter: r.Title, Category: r.Category, Author: r.Author,
			UpdateTime: ts,
		})
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

func hasRankCategory(gender, id int) bool {
	for _, c := range RankCategories[gender] {
		if c.ID == id {
			return true
		}
	}
	return false
}

// CategoryName 查找分类名。
func CategoryName(gender, id int) string {
	for _, c := range RankCategories[gender] {
		if c.ID == id {
			return c.Name
		}
	}
	return fmt.Sprintf("分类%d", id)
}

// HomeData 是首页聚合数据。
type HomeData struct {
	Recommends []RecommendBook `json:"recommends"`
	RankTop    []RankBook      `json:"rankTop"`
	RankName   string          `json:"rankName"`
	Recents    []RecentUpdate  `json:"recents"`
	FetchedAt  string          `json:"fetchedAt"`
}

// FetchHome 并发拉取首页所需的全部数据。
func (c *Client) FetchHome(ctx context.Context) (*HomeData, error) {
	var (
		wg      sync.WaitGroup
		recs    []RecommendBook
		rank    []RankBook
		recents []RecentUpdate
		recErr  error
		rankErr error
		updErr  error
	)
	wg.Add(3)
	go func() { defer wg.Done(); recs, recErr = c.FetchRecommend(ctx) }()
	go func() { defer wg.Done(); rank, _, rankErr = c.FetchRank(ctx, 1, 1141, 2, 0, 12) }()
	go func() { defer wg.Done(); recents, updErr = c.FetchRecentUpdate(ctx, 20) }()
	wg.Wait()

	if recErr != nil && rankErr != nil && updErr != nil {
		return nil, fmt.Errorf("首页数据全部获取失败: %v / %v / %v", recErr, rankErr, updErr)
	}
	return &HomeData{
		Recommends: recs,
		RankTop:    rank,
		RankName:   "男频阅读榜 · 西方奇幻",
		Recents:    recents,
		FetchedAt:  time.Now().Format(time.RFC3339),
	}, nil
}

// numToInt64 兼容接口里数字被序列化成字符串的情况。
func numToInt64(n json.Number) int64 {
	if n == "" {
		return 0
	}
	if v, err := n.Int64(); err == nil {
		return v
	}
	if f, err := n.Float64(); err == nil {
		return int64(f)
	}
	return 0
}

// TrimAbstract 截断简介。
func TrimAbstract(s string, n int) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
