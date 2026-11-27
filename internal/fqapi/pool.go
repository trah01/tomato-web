package fqapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// 设备池：多台匿名设备轮换取正文，命中风控时换设备重试。
//
// 设计对齐上游（fanqie-assistant / fanqie-novel-bypass）的设备池：
//
//   - 默认 3 个槽位；首槽同步注册，其余槽延迟 10s / 30s 注册，
//     避免「短时间内批量注册多台设备」本身触发 snssdk 风控。
//   - 每个槽独立记连续失败；连续 2 次失败判 dead，自动切到下一个 healthy 槽。
//   - 刚换过设备又失败时不再继续换（unverified 护栏）：此时问题多半不在设备，
//     继续换只是白注册，等有一次请求成功后再解锁。
//   - dead 槽 24h 内不补员；池子全 dead 时按上游做法延迟 5 分钟整体重置，
//     且 24h 内最多自动重置一次。
//   - 注册后顺手调 privilege/add 挂 30 天权益（失败忽略，见 privilege.go）。
//
// 与上游的差别：这是服务端常驻进程，没有前端按钮，所以额外提供
// Rotate / Refill / Reset 三个入口给 HTTP 管理接口用。
const (
	// DefaultPoolSize 是设备池默认槽位数。
	DefaultPoolSize = 3

	// poolFailureThreshold 是连续失败多少次判 dead（上游 FAILURE_THRESHOLD=2）。
	poolFailureThreshold = 2

	// poolRefillCooldown 是 dead 槽的补员冷却，防止连点触发批量注册风控。
	poolRefillCooldown = 24 * time.Hour

	// poolAutoResetDelay 是「全 dead」后延迟多久整体重置。
	poolAutoResetDelay = 5 * time.Minute

	// poolAutoResetThrottle 是自动重置节流窗口。
	poolAutoResetThrottle = 24 * time.Hour
)

// 槽位延迟注册时间表（首槽立即）。
var poolStagger = []time.Duration{0, 10 * time.Second, 30 * time.Second}

// ErrPoolExhausted 表示设备池里已经没有可用设备。
var ErrPoolExhausted = errors.New("fqapi: 设备池已耗尽")

// SlotHealth 是槽位健康状态。
type SlotHealth string

const (
	SlotHealthy SlotHealth = "healthy"
	SlotDead    SlotHealth = "dead"
)

// PoolSlot 是设备池里的一个槽位。
//
// 只持久化设备身份与健康状态：内容密钥与设备绑定、且按设计不落盘，
// 切到某个槽位时用 device_id + install_id 重新申请。
type PoolSlot struct {
	Index               int          `json:"index"`
	Device              *DeviceState `json:"device"`
	DeviceType          string       `json:"device_type,omitempty"`
	VIPExpireAt         int64        `json:"vip_expire_at,omitempty"`
	Health              SlotHealth   `json:"health"`
	FailureStreak       int          `json:"failure_streak"`
	LastFailureAt       int64        `json:"last_failure_at,omitempty"`
	LastSuccessAt       int64        `json:"last_success_at,omitempty"`
	RegisteredAt        int64        `json:"registered_at"`
	RefillCooldownUntil int64        `json:"refill_cooldown_until,omitempty"`
}

// PoolState 是设备池的持久化形态。
type PoolState struct {
	Slots           []*PoolSlot `json:"slots"`
	ActiveIndex     int         `json:"active_index"`
	LastAutoResetAt int64       `json:"last_auto_reset_at,omitempty"`
	AutoResetAt     int64       `json:"auto_reset_at,omitempty"`
}

// DevicePool 管理设备身份轮换。零值不可用，用 NewDevicePool 创建。
type DevicePool struct {
	client *Client
	path   string
	size   int
	logf   func(format string, args ...any)
	now    func() time.Time

	// provision 注册一台新设备（含权益激活），测试可替换。
	provision func(ctx context.Context) (*PoolSlot, error)
	// activate 把设备设为当前设备并申请内容密钥，测试可替换。
	activateFn func(ctx context.Context, st *DeviceState) error

	mu         sync.Mutex // 保护 state / unverified
	opMu       sync.Mutex // 串行化注册、补员、重置
	activateMu sync.Mutex // 串行化「切设备 + 申请密钥」

	// activating 表示此刻正在做「切设备 + 申请密钥」。
	// 内容接口发现设备为空时会回头找池子要设备，此标记用于避免重入死锁。
	activating atomic.Bool

	state      PoolState
	unverified bool
}

// NewDevicePool 创建设备池。size <= 0 时用默认值；日志函数可为 nil。
func NewDevicePool(client *Client, path string, size int) *DevicePool {
	if size <= 0 {
		size = DefaultPoolSize
	}
	if size > 10 {
		size = 10
	}
	p := &DevicePool{
		client: client,
		path:   path,
		size:   size,
		now:    time.Now,
	}
	p.provision = p.provisionUpstream
	p.activateFn = p.activateUpstream
	return p
}

// SetLogger 设置日志输出（通常传 logger.Printf）。
func (p *DevicePool) SetLogger(fn func(format string, args ...any)) {
	if fn != nil {
		p.logf = fn
	}
}

func (p *DevicePool) log(format string, args ...any) {
	if p.logf != nil {
		p.logf(format, args...)
	}
}

// ---- 持久化 ----

// Load 读取磁盘上的设备池；文件不存在时返回 nil。
func (p *DevicePool) Load() error {
	if p.path == "" {
		return nil
	}
	raw, err := os.ReadFile(p.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	var st PoolState
	if err := json.Unmarshal(raw, &st); err != nil {
		return err
	}
	p.mu.Lock()
	p.state = st
	p.normalizeLocked()
	p.mu.Unlock()
	return nil
}

// Save 把设备池写入磁盘（0600）。
func (p *DevicePool) Save() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.saveLocked()
}

func (p *DevicePool) saveLocked() error {
	if p.path == "" {
		return nil
	}
	if dir := filepath.Dir(p.path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	raw, err := json.MarshalIndent(p.state, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p.path, raw, 0o600)
}

// normalizeLocked 修正下标、补齐槽位序号、丢掉空槽。
func (p *DevicePool) normalizeLocked() {
	slots := make([]*PoolSlot, 0, len(p.state.Slots))
	for i, s := range p.state.Slots {
		if s == nil || s.Device == nil || s.Device.DeviceID == "" {
			continue
		}
		s.Index = i
		if s.Health == "" {
			s.Health = SlotHealthy
		}
		slots = append(slots, s)
	}
	p.state.Slots = slots
	if len(slots) == 0 {
		p.state.ActiveIndex = 0
		return
	}
	if p.state.ActiveIndex < 0 || p.state.ActiveIndex >= len(slots) {
		p.state.ActiveIndex = 0
	}
}

// AdoptDeviceFile 把旧版单设备身份（data/device.json）迁移成池子的第一个槽位。
//
// 池子已有设备时不做任何事，避免覆盖已有身份。
func (p *DevicePool) AdoptDeviceFile(path string) error {
	if path == "" {
		return nil
	}
	st, err := LoadDeviceFile(path)
	if err != nil {
		return err
	}
	if st == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.state.Slots) > 0 {
		return nil
	}
	p.state.Slots = []*PoolSlot{{
		Index:        0,
		Device:       st,
		DeviceType:   st.Model,
		Health:       SlotHealthy,
		RegisteredAt: p.now().UnixMilli(),
	}}
	p.state.ActiveIndex = 0
	if err := p.saveLocked(); err != nil {
		return err
	}
	p.log("设备池: 已从 %s 迁移设备 %s 到槽 0", path, st.DeviceID)
	return nil
}

// ---- 上游实现：注册 + 激活 + 切换 ----

// provisionUpstream 注册一台新设备并尝试挂 30 天权益。
func (p *DevicePool) provisionUpstream(ctx context.Context) (*PoolSlot, error) {
	dev := randomDevice()
	if err := p.client.registerDevice(ctx, dev); err != nil {
		return nil, fmt.Errorf("设备注册失败: %w", err)
	}
	st := stateFromDevice(dev)

	slot := &PoolSlot{
		Device:       st,
		DeviceType:   dev.Model,
		Health:       SlotHealthy,
		RegisteredAt: p.now().UnixMilli(),
	}
	// 权益是可选步骤：失败只记日志，不影响设备可用性。
	if exp, err := p.client.ActivatePremium(ctx, st); err != nil {
		p.log("设备池: 设备 %s 权益激活跳过: %v", st.DeviceID, err)
	} else if exp > 0 {
		slot.VIPExpireAt = exp
		p.log("设备池: 设备 %s 权益到期 %s", st.DeviceID, time.Unix(exp, 0).Format("2006-01-02"))
	}
	return slot, nil
}

// activateUpstream 把设备设为当前设备并申请内容密钥。
func (p *DevicePool) activateUpstream(ctx context.Context, st *DeviceState) error {
	// 已经是这台设备且密钥还在：不需要再切一次、再申请一次。
	if d := p.client.Device(); d != nil && d.DeviceID == st.DeviceID && p.client.ContentKey() != "" {
		return nil
	}
	p.client.LoadDevice(st)
	if err := p.client.EnsureKeyPure(ctx); err != nil {
		return fmt.Errorf("内容密钥申请失败: %w", err)
	}
	return nil
}

// activate 切换当前设备（串行，避免并发请求各自申请一次密钥）。
func (p *DevicePool) activate(ctx context.Context, st *DeviceState) error {
	p.activateMu.Lock()
	defer p.activateMu.Unlock()
	p.activating.Store(true)
	defer p.activating.Store(false)
	return p.activateFn(ctx, st)
}

// activatingNow 供 Client 判断是否已在池子的激活流程内，避免重入。
func (p *DevicePool) activatingNow() bool { return p.activating.Load() }

// ---- 查询 ----

// Size 返回槽位容量。
func (p *DevicePool) Size() int { return p.size }

// ActiveDevice 返回当前生效的设备身份（可能为 nil）。
func (p *DevicePool) ActiveDevice() *DeviceState {
	p.mu.Lock()
	defer p.mu.Unlock()
	slot := p.activeSlotLocked()
	if slot == nil || slot.Device == nil {
		return nil
	}
	st := *slot.Device
	return &st
}

// State 返回池状态快照（深拷贝，供管理接口/日志使用）。
func (p *DevicePool) State() PoolState {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := PoolState{
		ActiveIndex:     p.state.ActiveIndex,
		LastAutoResetAt: p.state.LastAutoResetAt,
		AutoResetAt:     p.state.AutoResetAt,
	}
	for _, s := range p.state.Slots {
		cp := *s
		if s.Device != nil {
			d := *s.Device
			cp.Device = &d
		}
		out.Slots = append(out.Slots, &cp)
	}
	return out
}

// Status 返回适合直接序列化成 JSON 的状态（不含任何密钥）。
func (p *DevicePool) Status() map[string]any {
	st := p.State()
	p.mu.Lock()
	unverified := p.unverified
	p.mu.Unlock()

	slots := make([]map[string]any, 0, len(st.Slots))
	healthy := 0
	for _, s := range st.Slots {
		if s.Health == SlotHealthy {
			healthy++
		}
		entry := map[string]any{
			"index":          s.Index,
			"deviceId":       "",
			"deviceType":     s.DeviceType,
			"health":         s.Health,
			"failureStreak":  s.FailureStreak,
			"lastSuccessAt":  s.LastSuccessAt,
			"lastFailureAt":  s.LastFailureAt,
			"registeredAt":   s.RegisteredAt,
			"refillCooldown": s.RefillCooldownUntil,
			"vipExpireAt":    s.VIPExpireAt,
		}
		if s.Device != nil {
			entry["deviceId"] = s.Device.DeviceID
		}
		slots = append(slots, entry)
	}

	out := map[string]any{
		"size":           p.size,
		"activeIndex":    st.ActiveIndex,
		"healthy":        healthy,
		"slots":          slots,
		"unverified":     unverified,
		"lastAutoReset":  st.LastAutoResetAt,
		"autoResetAt":    st.AutoResetAt,
		"failureLimit":   poolFailureThreshold,
		"refillCooldown": int64(poolRefillCooldown / time.Second),
	}
	if d := p.ActiveDevice(); d != nil {
		out["activeDeviceId"] = d.DeviceID
	}
	return out
}

// Summary 返回一行摘要，用于启动日志。
func (p *DevicePool) Summary() string {
	st := p.State()
	healthy := 0
	for _, s := range st.Slots {
		if s.Health == SlotHealthy {
			healthy++
		}
	}
	active := ""
	if st.ActiveIndex >= 0 && st.ActiveIndex < len(st.Slots) && st.Slots[st.ActiveIndex].Device != nil {
		active = st.Slots[st.ActiveIndex].Device.DeviceID
	}
	return fmt.Sprintf("%d/%d 健康，当前槽 %d（%s）", healthy, len(st.Slots), st.ActiveIndex, active)
}

func (p *DevicePool) activeSlotLocked() *PoolSlot {
	if p.state.ActiveIndex < 0 || p.state.ActiveIndex >= len(p.state.Slots) {
		return nil
	}
	return p.state.Slots[p.state.ActiveIndex]
}

// nextHealthyLocked 从当前槽的下一个开始环形找 healthy 槽，找不到返回 -1。
func (p *DevicePool) nextHealthyLocked() int {
	n := len(p.state.Slots)
	for offset := 1; offset <= n; offset++ {
		idx := (p.state.ActiveIndex + offset) % n
		if p.state.Slots[idx] != nil && p.state.Slots[idx].Health == SlotHealthy && p.state.Slots[idx].Device != nil {
			return idx
		}
	}
	return -1
}

// ---- 生命周期 ----

// Ensure 保证池子里有一台可用设备并已申请内容密钥。
//
// 启动时调用：已有 healthy 设备就直接复用，否则注册第一台。
func (p *DevicePool) Ensure(ctx context.Context) error {
	p.opMu.Lock()
	defer p.opMu.Unlock()

	p.mu.Lock()
	slot := p.activeSlotLocked()
	if slot != nil && slot.Health == SlotHealthy && slot.Device != nil {
		st := *slot.Device
		p.mu.Unlock()
		return p.activate(ctx, &st)
	}
	// 当前槽不可用但池子里还有别的健康设备：先切过去，不必重新注册。
	if next := p.nextHealthyLocked(); next >= 0 {
		p.state.ActiveIndex = next
		st := *p.state.Slots[next].Device
		_ = p.saveLocked()
		p.mu.Unlock()
		p.log("设备池: 当前槽不可用，切到槽 %d（%s）", next, st.DeviceID)
		return p.activate(ctx, &st)
	}
	p.mu.Unlock()

	return p.provisionFirstLocked(ctx)
}

// provisionFirstLocked 注册第一台设备（调用方需持有 opMu）。
func (p *DevicePool) provisionFirstLocked(ctx context.Context) error {
	slot, err := p.provision(ctx)
	if err != nil {
		return err
	}
	slot.Index = 0

	p.mu.Lock()
	if len(p.state.Slots) == 0 {
		p.state.Slots = append(p.state.Slots, slot)
	} else {
		p.state.Slots[0] = slot
	}
	p.state.ActiveIndex = 0
	p.unverified = false
	if err := p.saveLocked(); err != nil {
		p.mu.Unlock()
		return err
	}
	st := *slot.Device
	p.mu.Unlock()

	p.log("设备池: 槽 0 注册完成（%s）", st.DeviceID)
	return p.activate(ctx, &st)
}

// Fill 在后台补齐空槽位：槽 1 延迟 10s、槽 2 延迟 30s（对齐上游的错峰注册）。
//
// 只在启动时调用一次；ctx 用于进程退出时放弃后续注册。
func (p *DevicePool) Fill(ctx context.Context) {
	for i := 1; i < p.size; i++ {
		delay := time.Duration(i) * 30 * time.Second
		if i < len(poolStagger) {
			delay = poolStagger[i]
		}
		idx := i
		go func() {
			timer := time.NewTimer(delay)
			defer timer.Stop()
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
			}

			reqCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			if err := p.fillSlot(reqCtx, idx); err != nil {
				p.log("设备池: 槽 %d 注册失败: %v", idx, err)
				return
			}
			p.log("设备池: 槽 %d 已就绪（%s）", idx, p.Summary())
		}()
	}
}

// fillSlot 注册一台设备补到指定槽位（槽位已有设备、或池子已满时跳过）。
//
// 槽位数组始终保持紧凑：新设备追加到末尾，Index 字段只作展示，
// 真正的「当前槽」由 ActiveIndex（数组下标）决定。
func (p *DevicePool) fillSlot(ctx context.Context, index int) error {
	p.opMu.Lock()
	defer p.opMu.Unlock()

	p.mu.Lock()
	if index < len(p.state.Slots) && p.state.Slots[index] != nil && p.state.Slots[index].Device != nil {
		p.mu.Unlock()
		return nil
	}
	if len(p.state.Slots) >= p.size {
		p.mu.Unlock()
		return nil
	}
	p.mu.Unlock()

	slot, err := p.provision(ctx)
	if err != nil {
		return err
	}

	p.mu.Lock()
	slot.Index = len(p.state.Slots)
	p.state.Slots = append(p.state.Slots, slot)
	if p.state.ActiveIndex >= len(p.state.Slots) {
		p.state.ActiveIndex = 0
	}
	err = p.saveLocked()
	p.mu.Unlock()
	return err
}

// ReportSuccess 上报一次成功请求：重置当前槽的失败计数并解除 unverified 护栏。
func (p *DevicePool) ReportSuccess() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.unverified = false
	slot := p.activeSlotLocked()
	if slot == nil {
		return
	}
	if slot.FailureStreak != 0 || slot.LastSuccessAt == 0 {
		slot.FailureStreak = 0
		slot.LastSuccessAt = p.now().UnixMilli()
		_ = p.saveLocked()
	}
}

// Rotate 在设备被风控时调用：失败计数 +1，达到阈值就判 dead 并切到下一台。
//
// 返回 switched 表示是否真的换了设备；换了设备时当前 Client 已切到新身份
// 并重新申请好内容密钥，调用方可以直接重试。
//
// 池子全 dead 时返回 ErrPoolExhausted，并按上游做法调度 5 分钟后的整体重置。
func (p *DevicePool) Rotate(ctx context.Context, reason string) (bool, error) {
	p.mu.Lock()
	if p.unverified {
		// 刚换过设备又失败：问题多半不在设备（出口 IP、上游整体故障），
		// 再换只会白注册。等有一次请求成功后再解锁。
		p.mu.Unlock()
		p.log("设备池: 换设备后仍未成功，暂停轮换（%s）", truncateReason(reason))
		return false, nil
	}
	slot := p.activeSlotLocked()
	if slot == nil {
		p.mu.Unlock()
		if err := p.Ensure(ctx); err != nil {
			return false, err
		}
		return true, nil
	}

	slot.FailureStreak++
	slot.LastFailureAt = p.now().UnixMilli()
	if slot.FailureStreak < poolFailureThreshold {
		_ = p.saveLocked()
		p.mu.Unlock()
		p.log("设备池: 设备 %s 失败 %d/%d（%s）",
			slotDeviceID(slot), slot.FailureStreak, poolFailureThreshold, truncateReason(reason))
		return false, nil
	}

	slot.Health = SlotDead
	slot.RefillCooldownUntil = p.now().Add(poolRefillCooldown).UnixMilli()
	next := p.nextHealthyLocked()

	if next < 0 {
		_ = p.saveLocked()
		p.mu.Unlock()
		p.log("设备池: 全部槽位判 dead，%s 后尝试整体重置", poolAutoResetDelay)
		p.scheduleAutoReset()
		return false, ErrPoolExhausted
	}

	p.state.ActiveIndex = next
	p.unverified = true
	_ = p.saveLocked()
	st := *p.state.Slots[next].Device
	deadID := slotDeviceID(slot)
	p.mu.Unlock()

	p.log("设备池: 设备 %s 判 dead（%s），切换到槽 %d（%s）", deadID, truncateReason(reason), next, st.DeviceID)
	if err := p.activate(ctx, &st); err != nil {
		return true, err
	}
	return true, nil
}

// SwitchTo 手动切到指定槽位（目标必须 healthy）。
func (p *DevicePool) SwitchTo(ctx context.Context, index int) error {
	p.mu.Lock()
	if index < 0 || index >= len(p.state.Slots) || p.state.Slots[index] == nil {
		p.mu.Unlock()
		return fmt.Errorf("槽 %d 不存在", index)
	}
	slot := p.state.Slots[index]
	if slot.Health != SlotHealthy || slot.Device == nil {
		p.mu.Unlock()
		return fmt.Errorf("槽 %d 已 dead，请先补员", index)
	}
	p.state.ActiveIndex = index
	slot.FailureStreak = 0
	p.unverified = false
	_ = p.saveLocked()
	st := *slot.Device
	p.mu.Unlock()

	p.log("设备池: 手动切换到槽 %d（%s）", index, st.DeviceID)
	return p.activate(ctx, &st)
}

// Refill 在指定槽位重新注册一台设备（替换 dead 设备）。
//
// 与上游一致：槽位必须 dead，且 24h 补员冷却未结束时不补。
func (p *DevicePool) Refill(ctx context.Context, index int) error {
	p.opMu.Lock()
	defer p.opMu.Unlock()
	return p.refillLocked(ctx, index)
}

func (p *DevicePool) refillLocked(ctx context.Context, index int) error {
	p.mu.Lock()
	if index < 0 || index >= p.size {
		p.mu.Unlock()
		return fmt.Errorf("槽 %d 不存在（池容量 %d）", index, p.size)
	}
	if index >= len(p.state.Slots) || p.state.Slots[index] == nil {
		p.mu.Unlock()
		return fmt.Errorf("槽 %d 尚未注册", index)
	}
	slot := p.state.Slots[index]
	if slot.Health == SlotHealthy {
		p.mu.Unlock()
		return fmt.Errorf("槽 %d 健康，不需要补员", index)
	}
	if remain := slot.RefillCooldownUntil - p.now().UnixMilli(); remain > 0 {
		p.mu.Unlock()
		return fmt.Errorf("槽 %d 补员冷却中，还剩 %d 分钟", index, (remain+59999)/60000)
	}
	wasActive := p.state.ActiveIndex == index
	p.mu.Unlock()

	newSlot, err := p.provision(ctx)
	if err != nil {
		return err
	}
	newSlot.Index = index

	p.mu.Lock()
	p.state.Slots[index] = newSlot
	p.normalizeLocked()
	_ = p.saveLocked()
	st := *newSlot.Device
	p.mu.Unlock()

	p.log("设备池: 槽 %d 补员完成（%s）", index, st.DeviceID)
	if wasActive || p.ActiveDevice() == nil {
		if err := p.SwitchTo(ctx, index); err != nil {
			return err
		}
	}
	return nil
}

// Reset 清空并重建整个池子（自动恢复与手动重置共用）。
func (p *DevicePool) Reset(ctx context.Context) error {
	p.opMu.Lock()
	defer p.opMu.Unlock()
	return p.resetLocked(ctx)
}

func (p *DevicePool) resetLocked(ctx context.Context) error {
	p.mu.Lock()
	p.state = PoolState{LastAutoResetAt: p.now().UnixMilli()}
	p.unverified = false
	_ = p.saveLocked()
	p.mu.Unlock()

	p.log("设备池: 已清空，开始重新注册")
	return p.provisionFirstLocked(ctx)
}

// scheduleAutoReset 在池子全 dead 时调度一次延迟重置（24h 内最多一次）。
func (p *DevicePool) scheduleAutoReset() {
	p.mu.Lock()
	now := p.now()
	if p.state.AutoResetAt > now.UnixMilli() {
		p.mu.Unlock()
		return
	}
	if last := p.state.LastAutoResetAt; last > 0 && now.Sub(time.UnixMilli(last)) < poolAutoResetThrottle {
		p.mu.Unlock()
		p.log("设备池: 24h 内已自动重置过，跳过")
		return
	}
	p.state.AutoResetAt = now.Add(poolAutoResetDelay).UnixMilli()
	_ = p.saveLocked()
	delay := time.Until(time.UnixMilli(p.state.AutoResetAt))
	p.mu.Unlock()

	go func() {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		<-timer.C

		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		if err := p.Reset(ctx); err != nil {
			p.log("设备池: 自动重置失败: %v", err)
			return
		}
		p.log("设备池: 自动重置完成（%s）", p.Summary())
	}()
}

// ---- 小工具 ----

func slotDeviceID(s *PoolSlot) string {
	if s == nil || s.Device == nil {
		return ""
	}
	return s.Device.DeviceID
}

func truncateReason(reason string) string {
	reason = strings.TrimSpace(reason)
	if len(reason) > 120 {
		return reason[:120] + "…"
	}
	return reason
}
