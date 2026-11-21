package fqapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

// 正文清洗与网页端通道。
//
// 这些能力被阅读接口、预取和正文通道共用，因此与下载功能解耦，单独放这里。

var (
	reTags     = regexp.MustCompile(`<[^>]+>`)
	reSpaces   = regexp.MustCompile(`[ \t\p{Zs}]+`)
	rePGCVoice = regexp.MustCompile(`\{!--\s*PGC_VOICE:.*?--\}`)
	reFmAsr    = regexp.MustCompile(`(?s)<[^>]*novel-fm-asr[^>]*>.*?</[^>]+>`)
)

// CleanContent 把章节 HTML 清洗成纯文本段落。
func CleanContent(html string) string {
	s := reFmAsr.ReplaceAllString(html, "")
	s = rePGCVoice.ReplaceAllString(s, "")
	// 段落分隔：把块级结束标签换成换行
	for _, tag := range []string{"</p>", "</div>", "<br>", "<br/>", "<br />"} {
		s = strings.ReplaceAll(s, tag, "\n")
	}
	s = reTags.ReplaceAllString(s, "")
	s = strings.ReplaceAll(s, "&nbsp;", " ")
	s = strings.ReplaceAll(s, "&amp;", "&")
	s = strings.ReplaceAll(s, "&lt;", "<")
	s = strings.ReplaceAll(s, "&gt;", ">")
	s = strings.ReplaceAll(s, "&quot;", `"`)
	s = reSpaces.ReplaceAllString(s, "")

	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	for _, ln := range lines {
		ln = strings.TrimSpace(ln)
		if ln != "" {
			out = append(out, ln)
		}
	}
	// 段落之间用空行分隔（\n\n），便于前端与 TXT 统一处理
	return strings.Join(out, "\n\n")
}

// ---- 网页端兜底接口 ----

// WebChapter 尝试直接从网页接口取章节（免费章可用，付费章会被截断）。
func (c *Client) WebChapter(ctx context.Context, itemID string) (string, bool, error) {
	u := baseWeb + "/api/reader/full?itemId=" + url.QueryEscape(itemID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", false, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36")
	req.Header.Set("Referer", baseWeb+"/reader/"+url.QueryEscape(itemID))
	req.Header.Set("Accept", "application/json, text/plain, */*")
	if ck := c.Cookie(); ck != "" {
		req.Header.Set("Cookie", ck)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return "", false, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	var jr struct {
		Code int `json:"code"`
		Data struct {
			ChapterData struct {
				Content       string `json:"content"`
				IsChapterLock bool   `json:"isChapterLock"`
			} `json:"chapterData"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &jr); err != nil {
		return "", false, &APIError{Status: resp.StatusCode, Body: string(raw), Msg: "网页章节解析失败"}
	}
	cd := jr.Data.ChapterData
	return cd.Content, cd.IsChapterLock, nil
}
