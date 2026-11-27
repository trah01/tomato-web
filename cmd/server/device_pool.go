package main

import (
	"context"
	"encoding/json"
	"net/http"
	"time"
)

// 设备池管理接口。
//
// 设备池的常规动作是自动的：内容接口命中 403/风控空响应时自动换设备。
// 这里额外暴露手动入口，方便排查与恢复（对应上游控制面板的「切换到槽 N /
// 补员 / 重置池子」三个按钮）。
//
//	GET  /api/device/pool           查看池状态
//	POST /api/device/pool/rotate    手动轮换到下一台健康设备；可带 {"index":N} 指定槽位
//	POST /api/device/pool/refill    给指定槽位补一台新设备 {"index":N}
//	POST /api/device/pool/reset     清空并重建整个池子

// handleDevicePool 返回设备池状态。
func (s *server) handleDevicePool(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"enabled": false,
			"detail":  "未启用设备池（-device-pool 为空）",
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled": true,
		"pool":    s.pool.Status(),
	})
}

// handleDevicePoolRotate 手动轮换设备。
func (s *server) handleDevicePoolRotate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "仅支持 POST"})
		return
	}
	if s.pool == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "未启用设备池"})
		return
	}
	var req struct {
		Index *int `json:"index"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	if req.Index != nil {
		if err := s.pool.SwitchTo(ctx, *req.Index); err != nil {
			writeErr(w, http.StatusBadRequest, "切换设备失败", err)
			return
		}
	} else {
		// 不带下标时按「当前设备失败」处理，由池子决定是否切到下一台。
		if _, err := s.pool.Rotate(ctx, "手动轮换"); err != nil {
			writeErr(w, http.StatusBadGateway, "轮换设备失败", err)
			return
		}
	}
	s.saveDevice()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "pool": s.pool.Status()})
}

// handleDevicePoolRefill 给 dead 槽位补一台新设备。
func (s *server) handleDevicePoolRefill(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "仅支持 POST"})
		return
	}
	if s.pool == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "未启用设备池"})
		return
	}
	var req struct {
		Index *int `json:"index"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Index == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "需要 index"})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	if err := s.pool.Refill(ctx, *req.Index); err != nil {
		writeErr(w, http.StatusBadGateway, "补员失败", err)
		return
	}
	s.saveDevice()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "pool": s.pool.Status()})
}

// handleDevicePoolReset 清空并重建整个设备池。
func (s *server) handleDevicePoolReset(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "仅支持 POST"})
		return
	}
	if s.pool == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "未启用设备池"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	if err := s.pool.Reset(ctx); err != nil {
		writeErr(w, http.StatusBadGateway, "重置设备池失败", err)
		return
	}
	s.saveDevice()
	s.pool.Fill(context.Background())
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "pool": s.pool.Status()})
}
