package fqapi

import (
	"context"
	"errors"
	"sync"
	"time"
)

// PrefetchCache 是按需预取章节正文的缓存。
//
// 阅读场景不需要整本下载：只要保证「当前章附近若干章」已就绪即可。
// 策略：
//   - 读到第 i 章时，确保 [i-window, i+window] 区间有内容
//   - 后台串行抓取，控制频率避免风控
//   - 已抓到的章节永久缓存（含失败标记，带冷却时间后才重试）
//
// rateLimitCooldown 是命中风控后暂停预取的时长。
//
// 签名设备被限流后继续抓取只会延长限制时间，所以整体冷却一段时间。
const rateLimitCooldown = 10 * time.Minute

type PrefetchCache struct {
	source *ChapterSource

	mu            sync.RWMutex
	cache         map[string]*chapterEntry
	inFlight      map[string]bool
	order         []string // LRU 顺序（最近访问的 key 在前）
	cooldownUntil time.Time

	queue   chan prefetchJob
	workers int
	delay   time.Duration

	maxEntries int
	window     int

	logger func(format string, args ...any)
}

type chapterEntry struct {
	BookID    string
	ItemID    string
	Title     string
	Content   string
	Index     int
	WordCount int
	Source    string
	Err       string
	FailedAt  time.Time
	Ready     bool
}

type prefetchJob struct {
	bookID string
	ch     ChapterMeta
}

// PrefetchOptions 配置预取缓存。
type PrefetchOptions struct {
	Window     int           // 前后各预取几章
	Workers    int           // 并发抓取数（建议 1）
	Delay      time.Duration // 每章间隔
	MaxEntries int           // 缓存上限
	Logger     func(format string, args ...any)
}

// NewPrefetchCache 创建预取缓存。
func NewPrefetchCache(local *Client, decoder interface{ Decode(string) string }, opt PrefetchOptions) *PrefetchCache {
	if opt.Window <= 0 {
		opt.Window = 3
	}
	if opt.Workers <= 0 {
		opt.Workers = 1
	}
	if opt.Delay <= 0 {
		opt.Delay = 1500 * time.Millisecond
	}
	if opt.MaxEntries <= 0 {
		opt.MaxEntries = 200
	}
	logf := opt.Logger
	if logf == nil {
		logf = func(string, ...any) {}
	}

	p := &PrefetchCache{
		source:     NewChapterSource(local, decoder),
		cache:      make(map[string]*chapterEntry),
		inFlight:   make(map[string]bool),
		queue:      make(chan prefetchJob, 256),
		workers:    opt.Workers,
		delay:      opt.Delay,
		maxEntries: opt.MaxEntries,
		window:     opt.Window,
		logger:     logf,
	}
	for i := 0; i < p.workers; i++ {
		go p.worker()
	}
	return p
}

// SetWindow 运行时调整预取窗口。
func (p *PrefetchCache) SetWindow(n int) {
	if n < 0 {
		n = 0
	}
	p.mu.Lock()
	p.window = n
	p.mu.Unlock()
}

// Window 返回当前窗口大小。
func (p *PrefetchCache) Window() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.window
}

// Request 请求某一章，若已缓存则立即返回；否则入队并返回 ok=false。
// 调用方可以轮询 Get 或使用 WaitFor。
func (p *PrefetchCache) Request(bookID string, ch ChapterMeta) (entry *chapterEntry, ready bool) {
	key := ch.ItemID

	if p.paused() {
		return nil, false
	}

	p.mu.Lock()
	e, exists := p.cache[key]
	if exists && e.Ready {
		p.touchLocked(key)
		p.mu.Unlock()
		return e, true
	}
	// 失败项冷却 60 秒后才重试
	if exists && e.Err != "" && time.Since(e.FailedAt) < 60*time.Second {
		p.mu.Unlock()
		return e, false
	}
	needQueue := !p.inFlight[key]
	if needQueue {
		p.inFlight[key] = true
	}
	p.mu.Unlock()

	if needQueue {
		p.enqueue(prefetchJob{bookID: bookID, ch: ch})
	}
	return e, false
}

// PrefetchAround 预取当前章前后 window 章。
func (p *PrefetchCache) PrefetchAround(bookID string, chapters []ChapterMeta, currentIdx int) {
	if p.paused() {
		return
	}
	w := p.Window()
	lo := currentIdx - w
	hi := currentIdx + w
	if lo < 0 {
		lo = 0
	}
	if hi >= len(chapters) {
		hi = len(chapters) - 1
	}
	// 先近后远，当前章优先
	order := []int{}
	for d := 0; d <= w; d++ {
		if currentIdx+d <= hi {
			order = append(order, currentIdx+d)
		}
		if currentIdx-d >= lo && d > 0 {
			order = append(order, currentIdx-d)
		}
	}
	for _, i := range order {
		if i < 0 || i >= len(chapters) {
			continue
		}
		ch := chapters[i]
		p.mu.Lock()
		e, exists := p.cache[ch.ItemID]
		if exists && (e.Ready || (e.Err != "" && time.Since(e.FailedAt) < 60*time.Second)) {
			p.mu.Unlock()
			continue
		}
		needQueue := !p.inFlight[ch.ItemID]
		if needQueue {
			p.inFlight[ch.ItemID] = true
		}
		p.mu.Unlock()
		if needQueue {
			p.enqueue(prefetchJob{bookID: bookID, ch: ch})
		}
	}
}

// WaitFor 等待某一章就绪，超时返回当前状态。
func (p *PrefetchCache) WaitFor(ctx context.Context, itemID string, timeout time.Duration) (*chapterEntry, error) {
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(150 * time.Millisecond)
	defer ticker.Stop()

	for {
		p.mu.RLock()
		e, ok := p.cache[itemID]
		p.mu.RUnlock()

		if ok && e.Ready {
			return e, nil
		}
		if ok && e.Err != "" {
			return e, errors.New(e.Err)
		}
		if time.Now().After(deadline) {
			return e, errors.New("等待章节超时")
		}
		select {
		case <-ctx.Done():
			return e, ctx.Err()
		case <-ticker.C:
		}
	}
}

// Get 立即查询缓存。
func (p *PrefetchCache) Get(itemID string) (*chapterEntry, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	e, ok := p.cache[itemID]
	if ok && e.Ready {
		return e, true
	}
	return e, false
}

// Status 返回缓存概况。
func (p *PrefetchCache) Status() map[string]any {
	p.mu.RLock()
	defer p.mu.RUnlock()
	ready, failed, inflight := 0, 0, 0
	for _, e := range p.cache {
		if e.Ready {
			ready++
		} else if e.Err != "" {
			failed++
		}
	}
	for range p.inFlight {
		inflight++
	}
	return map[string]any{
		"ready":       ready,
		"failed":      failed,
		"inflight":    inflight,
		"total":       len(p.cache),
		"window":      p.window,
		"delayMs":     p.delay.Milliseconds(),
		"rateLimited": time.Now().Before(p.cooldownUntil),
	}
}

// ---- 内部 ----

func (p *PrefetchCache) enqueue(j prefetchJob) {
	select {
	case p.queue <- j:
	default:
		// 队列满，放弃（下次访问会重新入队）
		p.mu.Lock()
		delete(p.inFlight, j.ch.ItemID)
		p.mu.Unlock()
	}
}

func (p *PrefetchCache) worker() {
	for j := range p.queue {
		p.process(j)
	}
}

func (p *PrefetchCache) process(j prefetchJob) {
	itemID := j.ch.ItemID

	// 限流冷却期内不再发起请求，避免加长限制时间
	if p.paused() {
		p.markFailed(itemID, "番茄签名被限流，稍后重试")
		return
	}

	// 限速：每章之间间隔
	time.Sleep(p.delay)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	content, source, err := p.source.Fetch(ctx, j.bookID, itemID)
	if err == nil {
		content = StripTitlePrefix(j.ch.Title, content)
	}
	if err != nil && IsRateLimited(err) {
		p.coolDown()
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	entry := &chapterEntry{
		BookID: j.bookID,
		ItemID: itemID,
		Title:  j.ch.Title,
		Index:  j.ch.Index,
		Source: source,
	}
	if err != nil {
		entry.Err = err.Error()
		entry.FailedAt = time.Now()
		p.logger("✗ 预取失败 %s: %v", j.ch.Title, err)
	} else {
		entry.Content = content
		entry.Ready = true
		entry.WordCount = len([]rune(content))
		p.logger("✓ 预取完成 %s (%s, %d 字)", j.ch.Title, source, entry.WordCount)
	}
	p.cache[itemID] = entry
	delete(p.inFlight, itemID)
	p.order = append([]string{itemID}, p.order...)
	p.evictLocked()
}

// paused 报告预取是否处于限流冷却期。
func (p *PrefetchCache) paused() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return time.Now().Before(p.cooldownUntil)
}

// coolDown 命中风控后进入冷却期。
func (p *PrefetchCache) coolDown() {
	p.mu.Lock()
	if until := time.Now().Add(rateLimitCooldown); until.After(p.cooldownUntil) {
		p.cooldownUntil = until
	}
	p.mu.Unlock()
	p.logger("⚠ 番茄签名被限流，预取暂停 %s", rateLimitCooldown)
}

// ResetRateLimit 解除限流冷却并清掉失败标记，让下一次访问立刻重试。
func (p *PrefetchCache) ResetRateLimit() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cooldownUntil = time.Time{}
	for _, e := range p.cache {
		if e.Err != "" {
			e.Err = ""
			e.FailedAt = time.Time{}
		}
	}
}

// markFailed 记录一次未发请求的跳过（不占用网络）。
func (p *PrefetchCache) markFailed(itemID, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.inFlight, itemID)
	entry := p.cache[itemID]
	if entry == nil {
		entry = &chapterEntry{ItemID: itemID}
		p.cache[itemID] = entry
	}
	entry.Err = reason
	entry.FailedAt = time.Now()
}

func (p *PrefetchCache) touchLocked(key string) {
	for i, k := range p.order {
		if k == key {
			p.order = append(p.order[:i], p.order[i+1:]...)
			break
		}
	}
	p.order = append([]string{key}, p.order...)
}

func (p *PrefetchCache) evictLocked() {
	for len(p.order) > p.maxEntries {
		oldest := p.order[len(p.order)-1]
		p.order = p.order[:len(p.order)-1]
		if e, ok := p.cache[oldest]; ok && e.Ready {
			delete(p.cache, oldest)
		}
	}
}
