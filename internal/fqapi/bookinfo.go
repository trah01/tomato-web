package fqapi

import (
	"context"
	"io"
	"net/http"
	"regexp"
	"strings"
)

var (
	reBookName   = regexp.MustCompile(`"bookName"\s*:\s*"([^"]+)"`)
	reAuthorName = regexp.MustCompile(`class="author-name-text"[^>]*>([^<]+)<`)
	reH1         = regexp.MustCompile(`(?s)<h1[^>]*>(.*?)</h1>`)
	reTag        = regexp.MustCompile(`<[^>]+>`)
)

// FetchBookMeta 从书籍详情页抓书名与作者（免签）。
func (c *Client) FetchBookMeta(ctx context.Context, bookID string) (title, author string, err error) {
	u := baseWeb + "/page/" + bookID
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36")
	req.Header.Set("Referer", baseWeb+"/")

	resp, err := c.http.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	html := string(raw)

	if m := reBookName.FindStringSubmatch(html); len(m) > 1 {
		title = strings.TrimSpace(m[1])
	}
	if title == "" {
		if m := reH1.FindStringSubmatch(html); len(m) > 1 {
			title = strings.TrimSpace(reTag.ReplaceAllString(m[1], ""))
		}
	}
	if m := reAuthorName.FindStringSubmatch(html); len(m) > 1 {
		author = strings.TrimSpace(m[1])
	}
	return title, author, nil
}
