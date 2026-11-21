package fqapi

import (
	"context"
	"errors"
	"strings"
)

// 正文来源标识（返回给前端展示与排障用）。
const (
	SourcePureSign   = "pure-sign"   // 纯算签名 + App 内容接口
	SourceAppChannel = "app-channel" // 同一条 App 接口，走另一组参数
	SourceWebChannel = "web-channel" // 网页接口（免签）
)

// previewLimit 是网页通道对锁定章节返回的预览字数上限。
const previewLimit = 120

// ChapterSource 把章节取数通道集中到一处，避免每个调用方各写一套回退顺序。
//
// 顺序：纯算签名通道 → App 接口备用参数 → 网页免签通道。
type ChapterSource struct {
	Local   *Client
	Decoder interface{ Decode(string) string }
}

// NewChapterSource 创建取章器。local 为空时只做清洗不做取数。
func NewChapterSource(local *Client, decoder interface{ Decode(string) string }) *ChapterSource {
	return &ChapterSource{Local: local, Decoder: decoder}
}

// SourceError 汇总所有通道的失败原因，并标记是否命中番茄风控限流。
type SourceError struct {
	Reasons     []string
	RateLimited bool
}

func (e *SourceError) Error() string {
	if len(e.Reasons) == 0 {
		return "章节取数失败"
	}
	return strings.Join(e.Reasons, " / ")
}

// IsRateLimited 判断错误是否表示番茄侧风控限流（此时立刻重试只会加重限制）。
func IsRateLimited(err error) bool {
	if err == nil {
		return false
	}
	var se *SourceError
	if errors.As(err, &se) {
		return se.RateLimited
	}
	return containsRateHint(err.Error())
}

var rateHints = []string{"风控", "限流", "ILLEGAL_ACCESS", "非法访问", "设备需更新"}

func containsRateHint(s string) bool {
	for _, k := range rateHints {
		if strings.Contains(s, k) {
			return true
		}
	}
	return false
}

// blockedResponse 判断上游是否把请求直接丢弃（空响应或命中风控提示）。
func blockedResponse(err error) bool {
	if err == nil {
		return false
	}
	var ae *APIError
	if errors.As(err, &ae) {
		if strings.TrimSpace(ae.Body) == "" && ae.Status >= 200 && ae.Status < 300 {
			return true
		}
		return containsRateHint(ae.Msg) || containsRateHint(ae.Body)
	}
	return containsRateHint(err.Error())
}

// Fetch 依次尝试各通道，返回已解密、已做字体还原的正文与来源。
func (cs *ChapterSource) Fetch(ctx context.Context, bookID, itemID string) (string, string, error) {
	fail := &SourceError{}
	if cs.Local == nil {
		return "", "", &SourceError{Reasons: []string{"未配置数据源"}}
	}

	// 1) 纯算签名通道
	html, err := cs.Local.FetchContentPure(ctx, bookID, itemID)
	switch {
	case err != nil:
		fail.Reasons = append(fail.Reasons, "纯算通道: "+err.Error())
		fail.RateLimited = fail.RateLimited || blockedResponse(err)
	case strings.TrimSpace(CleanContent(html)) == "":
		fail.Reasons = append(fail.Reasons, "纯算通道: 正文为空")
	default:
		return cs.decode(CleanContent(html)), SourcePureSign, nil
	}

	// 2) App 接口备用参数（同一套签名，换 book_id / 批量端点）
	if key := cs.Local.ContentKey(); key != "" {
		ciphers, err := cs.Local.BatchFull(ctx, []string{itemID}, bookID)
		if err != nil {
			fail.Reasons = append(fail.Reasons, "App 通道: "+err.Error())
			fail.RateLimited = fail.RateLimited || blockedResponse(err)
		} else if enc := ciphers[itemID]; enc != "" {
			if plain, e := DecryptChapter(enc, key); e == nil && strings.TrimSpace(plain) != "" {
				return cs.decode(CleanContent(plain)), SourceAppChannel, nil
			}
		}
	}

	// 3) 网页接口，免签，但锁定章节只有预览
	html, locked, err := cs.Local.WebChapter(ctx, itemID)
	switch {
	case err != nil:
		fail.Reasons = append(fail.Reasons, "网页通道: "+err.Error())
		fail.RateLimited = fail.RateLimited || blockedResponse(err)
	case strings.TrimSpace(CleanContent(html)) == "":
		fail.Reasons = append(fail.Reasons, "网页通道: 正文为空")
		fail.RateLimited = fail.RateLimited || locked
	case locked && len([]rune(CleanContent(html))) < previewLimit:
		fail.Reasons = append(fail.Reasons, "网页通道: 该章节仅返回预览")
	default:
		return cs.decode(CleanContent(html)), SourceWebChannel, nil
	}

	return "", "", fail
}

func (cs *ChapterSource) decode(s string) string {
	if cs.Decoder == nil {
		return s
	}
	return cs.Decoder.Decode(s)
}

// StripTitlePrefix 去掉正文开头重复的章节标题。
//
// 番茄正文 HTML 里带 <h1>标题</h1>，清洗成纯文本后标题会粘在第一段前面，
// 与页面标题、TXT 的章标题重复。这里逐字符比对（忽略空白），命中才裁剪。
func StripTitlePrefix(title, content string) string {
	want := strings.Join(strings.Fields(title), "")
	if want == "" || content == "" {
		return content
	}
	ti := 0
	i := 0
	for ; i < len(content) && ti < len(want); i++ {
		c := content[i]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			continue
		}
		if c != want[ti] {
			return content
		}
		ti++
	}
	if ti < len(want) {
		return content
	}
	return strings.TrimLeft(content[i:], " \t\r\n\u3000\u00a0")
}
