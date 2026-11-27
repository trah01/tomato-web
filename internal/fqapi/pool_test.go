package fqapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// newTestPool 造一个不碰网络的设备池：注册返回递增的假 device_id。
func newTestPool(t *testing.T, size int) (*DevicePool, *atomic.Int32) {
	t.Helper()
	client := New(Options{Timeout: 2})
	p := NewDevicePool(client, filepath.Join(t.TempDir(), "pool.json"), size)
	p.SetLogger(t.Logf)

	var seq atomic.Int32
	p.provision = func(ctx context.Context) (*PoolSlot, error) {
		n := seq.Add(1)
		dev := &DeviceState{
			DeviceID:  fmt.Sprintf("%016d", 1000000+int(n)),
			InstallID: fmt.Sprintf("%016d", 2000000+int(n)),
			Model:     "Pixel 4 XL",
		}
		return &PoolSlot{
			Device:       dev,
			DeviceType:   dev.Model,
			Health:       SlotHealthy,
			RegisteredAt: time.Now().UnixMilli(),
		}, nil
	}
	p.activateFn = func(ctx context.Context, st *DeviceState) error {
		client.LoadDevice(st)
		return nil
	}
	return p, &seq
}

func mustFill(t *testing.T, p *DevicePool) {
	t.Helper()
	ctx := context.Background()
	if err := p.Ensure(ctx); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	for i := 1; i < p.Size(); i++ {
		if err := p.fillSlot(ctx, i); err != nil {
			t.Fatalf("fillSlot(%d): %v", i, err)
		}
	}
}

func TestPoolEnsureAndFill(t *testing.T) {
	p, _ := newTestPool(t, 3)
	mustFill(t, p)

	st := p.State()
	if len(st.Slots) != 3 {
		t.Fatalf("槽位数 = %d, 期望 3", len(st.Slots))
	}
	for i, s := range st.Slots {
		if s.Device == nil || s.Device.DeviceID == "" {
			t.Fatalf("槽 %d 没有设备", i)
		}
		if s.Health != SlotHealthy {
			t.Fatalf("槽 %d health = %q", i, s.Health)
		}
		if s.Index != i {
			t.Fatalf("槽 %d Index = %d", i, s.Index)
		}
	}
	if p.ActiveDevice() == nil {
		t.Fatal("Ensure 后应把槽 0 设为当前设备")
	}
}

func TestPoolRotateAfterFailureThreshold(t *testing.T) {
	p, _ := newTestPool(t, 3)
	mustFill(t, p)
	ctx := context.Background()

	first := p.ActiveDevice().DeviceID

	// 第一次失败只计数，不切设备。
	switched, err := p.Rotate(ctx, "HTTP 403")
	if err != nil || switched {
		t.Fatalf("第一次失败不该切换: switched=%v err=%v", switched, err)
	}
	if got := p.ActiveDevice().DeviceID; got != first {
		t.Fatalf("设备不该变: %s -> %s", first, got)
	}

	// 第二次失败达到阈值：标记 dead 并切到下一台。
	switched, err = p.Rotate(ctx, "HTTP 403")
	if err != nil || !switched {
		t.Fatalf("达到阈值应切换: switched=%v err=%v", switched, err)
	}
	second := p.ActiveDevice().DeviceID
	if second == first {
		t.Fatal("切换后设备应当变化")
	}
	if p.State().Slots[0].Health != SlotDead {
		t.Fatal("旧槽位应标记 dead")
	}
	if p.State().Slots[0].RefillCooldownUntil == 0 {
		t.Fatal("dead 槽位应记录补员冷却")
	}

	// 刚换过设备又失败：unverified 护栏生效，不再继续换。
	switched, err = p.Rotate(ctx, "HTTP 403")
	if err != nil || switched {
		t.Fatalf("unverified 期间不该继续换设备: switched=%v err=%v", switched, err)
	}
	if got := p.ActiveDevice().DeviceID; got != second {
		t.Fatalf("unverified 期间设备不该变: %s -> %s", second, got)
	}

	// 有一次成功后再失败，才可以继续换。
	p.ReportSuccess()
	if _, err := p.Rotate(ctx, "HTTP 403"); err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	switched, err = p.Rotate(ctx, "HTTP 403")
	if err != nil || !switched {
		t.Fatalf("恢复后应能再次切换: switched=%v err=%v", switched, err)
	}
	if p.State().ActiveIndex != 2 {
		t.Fatalf("应切到槽 2, 实际 %d", p.State().ActiveIndex)
	}
}

func TestPoolExhaustedSchedulesAutoReset(t *testing.T) {
	p, _ := newTestPool(t, 2)
	mustFill(t, p)
	ctx := context.Background()

	// 槽 0：连续两次失败 → 判 dead，切到槽 1。
	p.Rotate(ctx, "403")
	if switched, _ := p.Rotate(ctx, "403"); !switched {
		t.Fatal("槽 0 达到阈值应切换")
	}
	p.ReportSuccess() // 新设备有一次成功，解除 unverified

	// 槽 1：连续两次失败 → 判 dead，且没有 healthy 槽位可用。
	p.Rotate(ctx, "403")
	_, err := p.Rotate(ctx, "403")
	if !errors.Is(err, ErrPoolExhausted) {
		t.Fatalf("池子全 dead 应返回 ErrPoolExhausted, 实际 %v", err)
	}
	if p.State().AutoResetAt == 0 {
		t.Fatal("全 dead 后应调度自动重置")
	}
}

func TestPoolPersistAndReload(t *testing.T) {
	p, _ := newTestPool(t, 3)
	mustFill(t, p)
	if err := p.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, err := p.Rotate(context.Background(), "403"); err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	if _, err := p.Rotate(context.Background(), "403"); err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	if err := p.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	q := NewDevicePool(New(Options{}), p.path, 3)
	if err := q.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	a, b := p.State(), q.State()
	if len(a.Slots) != len(b.Slots) {
		t.Fatalf("槽位数不一致: %d vs %d", len(a.Slots), len(b.Slots))
	}
	if a.ActiveIndex != b.ActiveIndex {
		t.Fatalf("ActiveIndex 不一致: %d vs %d", a.ActiveIndex, b.ActiveIndex)
	}
	if b.Slots[0].Health != SlotDead {
		t.Fatalf("dead 状态没持久化: %q", b.Slots[0].Health)
	}
	if b.Slots[0].Device.DeviceID != a.Slots[0].Device.DeviceID {
		t.Fatal("device_id 没持久化")
	}
}

func TestPoolAdoptDeviceFile(t *testing.T) {
	dir := t.TempDir()
	legacy := filepath.Join(dir, "device.json")
	st := &DeviceState{DeviceID: "1234567890", InstallID: "987654321", Model: "ANA-AN00"}
	if err := SaveDeviceFile(legacy, st); err != nil {
		t.Fatalf("SaveDeviceFile: %v", err)
	}

	p := NewDevicePool(New(Options{}), filepath.Join(dir, "pool.json"), 3)
	p.activateFn = func(ctx context.Context, st *DeviceState) error { return nil }
	if err := p.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := p.AdoptDeviceFile(legacy); err != nil {
		t.Fatalf("AdoptDeviceFile: %v", err)
	}
	if got := p.ActiveDevice(); got == nil || got.DeviceID != "1234567890" {
		t.Fatalf("迁移失败: %+v", got)
	}

	// 已有设备时不再覆盖。
	st2 := &DeviceState{DeviceID: "555", InstallID: "666"}
	if err := SaveDeviceFile(legacy, st2); err != nil {
		t.Fatalf("SaveDeviceFile: %v", err)
	}
	if err := p.AdoptDeviceFile(legacy); err != nil {
		t.Fatalf("AdoptDeviceFile 二次: %v", err)
	}
	if got := p.ActiveDevice().DeviceID; got != "1234567890" {
		t.Fatalf("不应覆盖已有设备, 实际 %s", got)
	}
}

func TestPoolEnsureSwitchesToHealthySlot(t *testing.T) {
	p, _ := newTestPool(t, 3)
	mustFill(t, p)

	// 槽 0 判 dead，但槽 1/2 仍然健康：Ensure 应直接切过去，不重新注册。
	p.mu.Lock()
	p.state.Slots[0].Health = SlotDead
	p.mu.Unlock()

	before := len(p.State().Slots)
	if err := p.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if got := p.State().ActiveIndex; got != 1 {
		t.Fatalf("应切到槽 1, 实际 %d", got)
	}
	if after := len(p.State().Slots); after != before {
		t.Fatalf("不应新增槽位: %d -> %d", before, after)
	}
}

func TestPoolRefillAndSwitch(t *testing.T) {
	p, _ := newTestPool(t, 2)
	mustFill(t, p)
	ctx := context.Background()

	// 让槽 0 判 dead 并切到槽 1。
	p.ReportSuccess()
	p.Rotate(ctx, "403")
	p.Rotate(ctx, "403")
	if p.State().ActiveIndex != 1 {
		t.Fatalf("应在槽 1, 实际 %d", p.State().ActiveIndex)
	}

	// 冷却期内补员应被拒绝。
	if err := p.Refill(ctx, 0); err == nil {
		t.Fatal("冷却期内补员应失败")
	}

	// 手动清了冷却后可以补一台新设备，并切回去。
	p.mu.Lock()
	p.state.Slots[0].RefillCooldownUntil = 0
	p.mu.Unlock()

	if err := p.Refill(ctx, 0); err != nil {
		t.Fatalf("Refill: %v", err)
	}
	if s := p.State().Slots[0]; s.Health != SlotHealthy {
		t.Fatalf("补员后应为 healthy, 实际 %q", s.Health)
	}
	if err := p.SwitchTo(ctx, 0); err != nil {
		t.Fatalf("SwitchTo: %v", err)
	}
	if p.State().ActiveIndex != 0 {
		t.Fatalf("应切回槽 0, 实际 %d", p.State().ActiveIndex)
	}
}

func TestWithDeviceRotationRetriesOn403(t *testing.T) {
	p, _ := newTestPool(t, 2)
	mustFill(t, p)
	client := p.client
	client.UsePool(p)

	// 预置一次失败，下一次失败即触发换设备。
	p.mu.Lock()
	p.state.Slots[0].FailureStreak = 1
	p.mu.Unlock()

	var calls int
	err := client.withDeviceRotation(context.Background(), 3, func() error {
		calls++
		if calls == 1 {
			return &APIError{Status: http.StatusForbidden, Msg: "forbidden"}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("重试后应成功: %v", err)
	}
	if calls != 2 {
		t.Fatalf("调用次数 = %d, 期望 2", calls)
	}
	if p.State().ActiveIndex != 1 {
		t.Fatalf("应已切换到槽 1, 实际 %d", p.State().ActiveIndex)
	}
}

func TestBlockedResponseClassification(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{&APIError{Status: http.StatusForbidden, Msg: "x"}, true},
		{&APIError{Status: http.StatusOK, Body: "", Msg: ""}, true},
		{&APIError{Status: http.StatusOK, Body: `{"code":0}`, Msg: ""}, false},
		{&APIError{Status: http.StatusOK, Body: "", Msg: "网关返回空响应（签名未通过风控）"}, true},
		{errors.New("network down"), false},
		{nil, false},
	}
	for i, c := range cases {
		if got := blockedResponse(c.err); got != c.want {
			t.Fatalf("case %d: blockedResponse = %v, 期望 %v", i, got, c.want)
		}
	}
}

func TestActivatePremiumRequestShape(t *testing.T) {
	var gotQuery map[string]string
	var gotBody string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s", r.Method)
		}
		gotQuery = map[string]string{}
		for k := range r.URL.Query() {
			gotQuery[k] = r.URL.Query().Get(k)
		}
		buf := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(buf)
		gotBody = string(buf)
		_, _ = w.Write([]byte(`{"code":0,"data":{"expire_time":"1767225600"}}`))
	}))
	defer srv.Close()

	old := privilegeAddURL
	privilegeAddURL = srv.URL + "/reading/user/privilege/add/v"
	defer func() { privilegeAddURL = old }()

	c := New(Options{})
	exp, err := c.ActivatePremium(context.Background(), &DeviceState{
		DeviceID:  "7400000000000000001",
		InstallID: "7400000000000000002",
		Model:     "Pixel 4 XL",
	})
	if err != nil {
		t.Fatalf("ActivatePremium: %v", err)
	}
	if exp != 1767225600 {
		t.Fatalf("expire = %d", exp)
	}
	if gotQuery["device_id"] != "7400000000000000001" || gotQuery["iid"] != "7400000000000000002" {
		t.Fatalf("query 设备参数不对: %v", gotQuery)
	}
	if gotQuery["version_code"] != privilegeVersionCode || gotQuery["aid"] != "1967" {
		t.Fatalf("query 版本/ aid 不对: %v", gotQuery)
	}
	for _, want := range []string{
		`"privilege_id":7210376203117531962`,
		`"amount":2592000`,
		`"from":8`,
		`"add_count_daily":0`,
	} {
		if !strings.Contains(gotBody, want) {
			t.Fatalf("body 缺少 %s: %s", want, gotBody)
		}
	}
	// unique_key 必须是字符串形式的毫秒时间戳。
	var probe struct {
		UniqueKey string `json:"unique_key"`
	}
	if err := json.Unmarshal([]byte(gotBody), &probe); err != nil || probe.UniqueKey == "" {
		t.Fatalf("unique_key 不是非空字符串: %s (%v)", gotBody, err)
	}
}

func TestActivatePremiumBusinessError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":7,"message":"no privilege"}`))
	}))
	defer srv.Close()

	old := privilegeAddURL
	privilegeAddURL = srv.URL
	defer func() { privilegeAddURL = old }()

	if _, err := New(Options{}).ActivatePremium(context.Background(), &DeviceState{DeviceID: "1", InstallID: "2"}); err == nil {
		t.Fatal("业务错误码应返回错误")
	}
}
