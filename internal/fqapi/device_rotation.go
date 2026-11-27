package fqapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// 设备轮换钩子：内容接口命中 403 / 风控空响应时，交给设备池换设备后重试。
//
// 上游把「设备被作废」的表现描述得很具体：/reading/* 接口返回 HTTP 200 +
// 空响应体（content-length: 0），没有错误码可判断；偶尔是明确的 403。
// 只要命中这两类，就认为问题在设备身份而不是请求参数。

// UsePool 把设备池挂到客户端；传 nil 取消。
func (c *Client) UsePool(p *DevicePool) {
	c.mu.Lock()
	c.pool = p
	c.mu.Unlock()
}

func (c *Client) poolRef() *DevicePool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.pool
}

// bootstrapPoolDevice 在还没有设备时向设备池要一台。
//
// 启动时注册失败（断网、上游抖动）后，第一次取正文会走到这里补注册；
// 池子正在切设备/申请密钥时不重入，避免死锁。
func (c *Client) bootstrapPoolDevice(ctx context.Context) error {
	if c.Device() != nil {
		return nil
	}
	p := c.poolRef()
	if p == nil || p.activatingNow() {
		return nil
	}
	return p.Ensure(ctx)
}

// reportDeviceSuccess 告诉设备池：当前设备能正常出海。
func (c *Client) reportDeviceSuccess() {
	if p := c.poolRef(); p != nil {
		p.ReportSuccess()
	}
}

// rotateDevice 请求设备池换一台设备。
func (c *Client) rotateDevice(ctx context.Context, cause error) (bool, error) {
	p := c.poolRef()
	if p == nil {
		return false, nil
	}
	return p.Rotate(ctx, cause.Error())
}

// withDeviceRotation 执行一次依赖设备身份的请求；命中 403/风控时换设备重试。
//
// attempts 是总尝试次数：设备池的失败阈值是 2，所以连续两次失败才会真正换设备，
// 默认给 3 次足够覆盖「第一次失败计数 → 第二次判 dead 并切换 → 第三次成功」。
func (c *Client) withDeviceRotation(ctx context.Context, attempts int, fn func() error) error {
	if attempts < 1 {
		attempts = 1
	}
	var lastErr error
	for i := 0; i < attempts; i++ {
		err := fn()
		if err == nil {
			c.reportDeviceSuccess()
			return nil
		}
		lastErr = err

		if c.poolRef() == nil || !blockedResponse(err) {
			return err
		}
		switched, rerr := c.rotateDevice(ctx, err)
		if rerr != nil {
			return fmt.Errorf("%w（换设备后仍失败: %v）", err, rerr)
		}
		if !switched {
			return err
		}
	}
	return lastErr
}

// deviceRejected 判断错误是否表示「设备身份被上游拒绝」。
func deviceRejected(err error) bool {
	if err == nil {
		return false
	}
	var ae *APIError
	if errors.As(err, &ae) {
		return ae.Status == http.StatusForbidden
	}
	return strings.Contains(err.Error(), "403")
}
