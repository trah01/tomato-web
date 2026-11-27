package fqapi

import (
	"context"
	"errors"
	"sync"
	"time"
)

// PrefetchCache 是按需取章的正文缓存。
//
// 阅读场景不需要整本下载：读到哪一章就取到哪一章，并把接下来的几章
// 合并成一次批量请求提前取回，翻页时直接命中缓存。
// 策略：
//   - 单章请求走 full 端点（正文最完整）
//   - 连续多章走 batch_full，一次请求取回多章
//   - 后台串行抓取，控制频率避免风控
//   - 已抓到的章节永久缓存（含失败标记，带冷却时间后才重试）
//
// rateLimitCooldown 是命中风控后暂停取章的时长。
//
// 签名设备被限流后继续抓取只会延长限制时间，所以整体冷却一段时间。
const rateLimitCooldown = 10 * time.Minute

// maxEntries 是正文缓存上限，超出按最久未用淘汰。
const maxEntries = 200

// defaultBatch 是一次批量请求最多带几章。
const defaultBatch = 3

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

	window     int
	batch      int
	maxEntries int
	lastFetch  time.Time

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

// prefetchJob 是一次取章任务，chs 可能包含多章（走批量端点）。
type prefetchJob struct {
	bookID string
	chs    []ChapterMeta
}

// PrefetchOptions 配置取章缓存。
type PrefetchOptions struct {
	Window     int           // 前后各取几章，0 表示只取当前章
	Workers    int           // 并发抓取数（建议 1）
	Delay      time.Duration // 两次请求之间最小间隔
	Batch      int           // 单次批量请求最多几章
	MaxEntries int           // 缓存上限
	Logger     func(format string, args ...any)
}

// NewPrefetchCache 创建取章缓存。
func NewPrefetchCache(local *Client, decoder interface{ Decode(string) string }, opt PrefetchOptions) *PrefetchCache {
	if opt.Workers <= 0 {
		opt.Workers = 1
	}
	if opt.Delay <= 0 {
		opt.Delay = 1500 * time.Millisecond
	}
	if opt.Batch <= 0 {
		opt.Batch = defaultBatch
	}
	if opt.MaxEntries <= 0 {
		opt.MaxEntries = maxEntries
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
		window:     opt.Window,
		batch:      opt.Batch,
		maxEntries: opt.MaxEntries,
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

// Batch 返回单次批量请求的章数上限。
func (p *PrefetchCache) Batch() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.batch
}

// Request 请求某一章：已缓存则立即返回 ready=true；
// 正在抓取或刚排队时返回 ready=false，调用方用 WaitFor 等待。
func (p *PrefetchCache) Request(bookID string, ch ChapterMeta) (entry *chapterEntry, ready bool) {
	key := ch.ItemID

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
	if time.Now().Before(p.cooldownUntil) {
		p.mu.Unlock()
		return e, false
	}
	needQueue := !p.inFlight[key]
	if needQueue {
		p.inFlight[key] = true
	}
	p.mu.Unlock()

	if needQueue {
		p.enqueue(prefetchJob{bookID: bookID, chs: []ChapterMeta{ch}})
	}
	return e, false
}

// WarmNext 把从 fromIdx 开始的 count 章合并成一次批量请求提前取回。
//
// 已经在缓存或排队中的章节会被跳过，避免重复请求。
func (p *PrefetchCache) WarmNext(bookID string, chapters []ChapterMeta, fromIdx, count int) int {
	if p.paused() {
		return 0
	}
	if count <= 0 {
		count = p.Batch()
	}
	if count > p.Batch() {
		count = p.Batch()
	}
	picked := make([]ChapterMeta, 0, count)
	p.mu.Lock()
	for i := fromIdx; i < len(chapters) && len(picked) < count; i++ {
		if i < 0 {
			continue
		}
		id := chapters[i].ItemID
		if e, ok := p.cache[id]; ok && e.Ready {
			continue
		}
		if p.inFlight[id] {
			continue
		}
		p.inFlight[id] = true
		picked = append(picked, chapters[i])
	}
	p.mu.Unlock()
	if len(picked) == 0 {
		return 0
	}
	p.enqueue(prefetchJob{bookID: bookID, chs: picked})
	return len(picked)
}

// WarmItemIDs 按 itemId 列表批量预取（前端拿不到 ChapterMeta 时使用）。
func (p *PrefetchCache) WarmItemIDs(bookID string, itemIDs []string) int {
	chs := make([]ChapterMeta, 0, len(itemIDs))
	for _, id := range itemIDs {
		if id != "" {
			chs = append(chs, ChapterMeta{ItemID: id})
		}
	}
	return p.WarmNext(bookID, chs, 0, len(chs))
}

// PrefetchAround 取当前章前后 window 章（window=0 时只取当前章）。
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
	picked := make([]ChapterMeta, 0, len(order))
	p.mu.Lock()
	for _, i := range order {
		if i < 0 || i >= len(chapters) {
			continue
		}
		ch := chapters[i]
		if e, ok := p.cache[ch.ItemID]; ok && e.Ready {
			continue
		}
		if p.inFlight[ch.ItemID] {
			continue
		}
		p.inFlight[ch.ItemID] = true
		picked = append(picked, ch)
	}
	p.mu.Unlock()

	// 按批量上限切分成若干次请求
	for len(picked) > 0 {
		n := p.Batch()
		if n > len(picked) {
			n = len(picked)
		}
		p.enqueue(prefetchJob{bookID: bookID, chs: picked[:n]})
		picked = picked[n:]
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
		queued := p.inFlight[itemID]
		cooling := time.Now().Before(p.cooldownUntil)
		p.mu.RUnlock()

		if ok && e.Ready {
			return e, nil
		}
		if ok && e.Err != "" {
			return e, errors.New(e.Err)
		}
		// 没有排队任务：多半是冷却中或队列已丢弃，不再空等。
		if !queued {
			if cooling {
				return e, errors.New("番茄风控冷却中，请稍后再试")
			}
			return e, errors.New("章节未进入取章队列")
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
	remain := int64(0)
	if left := time.Until(p.cooldownUntil); left > 0 {
		remain = int64(left / time.Second)
	}
	return map[string]any{
		"ready":           ready,
		"failed":          failed,
		"inflight":        inflight,
		"total":           len(p.cache),
		"window":          p.window,
		"batch":           p.batch,
		"delayMs":         p.delay.Milliseconds(),
		"rateLimited":     remain > 0,
		"cooldownSeconds": remain,
	}
}

// ---- 内部 ----

func (p *PrefetchCache) enqueue(j prefetchJob) {
	select {
	case p.queue <- j:
	default:
		// 队列满，放弃（下次访问会重新入队）
		p.mu.Lock()
		for _, ch := range j.chs {
			delete(p.inFlight, ch.ItemID)
		}
		p.mu.Unlock()
	}
}

func (p *PrefetchCache) worker() {
	for j := range p.queue {
		p.process(j)
	}
}

func (p *PrefetchCache) process(j prefetchJob) {
	// 过滤已就绪的章节，未就绪的才真正发请求
	todo := make([]ChapterMeta, 0, len(j.chs))
	p.mu.RLock()
	for _, ch := range j.chs {
		if e, ok := p.cache[ch.ItemID]; ok && e.Ready {
			continue
		}
		todo = append(todo, ch)
	}
	p.mu.RUnlock()
	if len(todo) == 0 {
		p.release(todo)
		return
	}

	// 限流冷却期内不再发起请求，避免加长限制时间
	if p.paused() {
		for _, ch := range todo {
			p.markFailed(ch.ItemID, "番茄风控冷却中，稍后重试")
		}
		return
	}

	// 多章：一次批量请求
	if len(todo) > 1 {
		ids := make([]string, 0, len(todo))
		byID := make(map[string]ChapterMeta, len(todo))
		for _, ch := range todo {
			ids = append(ids, ch.ItemID)
			byID[ch.ItemID] = ch
		}
		p.waitTurn()
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		got, err := p.source.FetchBatch(ctx, j.bookID, ids)
		cancel()

		if err != nil {
			if IsRateLimited(err) {
				p.coolDown()
			} else {
				p.logger("✗ 批量取章失败（%d 章）: %v", len(todo), err)
			}
		}
		for _, ch := range todo {
			if text, ok := got[ch.ItemID]; ok {
				p.finish(j.bookID, ch, text, SourceAppChannel, nil)
			}
		}
		// 批量里没回来的章节再逐章试一次
		pending := make([]ChapterMeta, 0, len(todo))
		for _, ch := range todo {
			if _, ok := got[ch.ItemID]; !ok {
				pending = append(pending, ch)
			}
		}
		todo = pending
	}

	for _, ch := range todo {
		if p.paused() {
			p.markFailed(ch.ItemID, "番茄风控冷却中，稍后重试")
			continue
		}
		p.fetchSingle(j.bookID, ch)
	}
}

func (p *PrefetchCache) fetchSingle(bookID string, ch ChapterMeta) {
	p.waitTurn()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	content, source, err := p.source.Fetch(ctx, bookID, ch.ItemID)
	p.finish(bookID, ch, content, source, err)
}

// finish 写入一条缓存结果（成功或失败），并释放 inFlight 标记。
func (p *PrefetchCache) finish(bookID string, ch ChapterMeta, content, source string, err error) {
	if err != nil && IsRateLimited(err) {
		p.coolDown()
	}
	if err == nil {
		content = StripTitlePrefix(ch.Title, content)
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	entry := &chapterEntry{
		BookID: bookID,
		ItemID: ch.ItemID,
		Title:  ch.Title,
		Index:  ch.Index,
		Source: source,
	}
	if err != nil {
		entry.Err = err.Error()
		entry.FailedAt = time.Now()
		p.logger("✗ 取章失败 %s: %v", ch.Title, err)
	} else {
		entry.Content = content
		entry.Ready = true
		entry.WordCount = len([]rune(content))
		p.logger("✓ 取章完成 %s (%s, %d 字)", ch.Title, source, entry.WordCount)
	}
	p.cache[ch.ItemID] = entry
	delete(p.inFlight, ch.ItemID)
	p.touchLocked(ch.ItemID)
	p.evictLocked()
}

func (p *PrefetchCache) release(chs []ChapterMeta) {
	p.mu.Lock()
	for _, ch := range chs {
		delete(p.inFlight, ch.ItemID)
	}
	p.mu.Unlock()
}

// waitTurn 保证两次上游请求之间至少间隔 delay，首次请求不额外等待。
func (p *PrefetchCache) waitTurn() {
	p.mu.RLock()
	last := p.lastFetch
	p.mu.RUnlock()
	if wait := p.delay - time.Since(last); wait > 0 {
		time.Sleep(wait)
	}
	p.mu.Lock()
	p.lastFetch = time.Now()
	p.mu.Unlock()
}

// Paused 报告取章是否处于限流冷却期。
func (p *PrefetchCache) Paused() bool {
	return p.paused()
}

// paused 报告取章是否处于限流冷却期。
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
	p.logger("⚠ 番茄签名被限流，取章暂停 %s", rateLimitCooldown)
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
