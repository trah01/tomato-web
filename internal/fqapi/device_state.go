package fqapi

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

// DeviceState 是可持久化的设备身份。
//
// 只保存注册得到的标识与展示用的机型字段；内容密钥不落盘，
// 每次启动用 device_id + install_id 重新申请（一次请求，不是重新注册）。
type DeviceState struct {
	DeviceID   string `json:"device_id"`
	InstallID  string `json:"install_id"`
	CDID       string `json:"cdid"`
	Brand      string `json:"brand"`
	Model      string `json:"model"`
	OSVersion  string `json:"os_version"`
	DPI        string `json:"dpi"`
	Resolution string `json:"resolution"`
}

// ExportDevice 导出当前设备身份，未注册时返回 nil。
func (c *Client) ExportDevice() *DeviceState {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.device == nil || c.device.DeviceID == "" {
		return nil
	}
	return stateFromDevice(c.device)
}

// stateFromDevice 把运行期的 Device 转成可持久化的 DeviceState。
func stateFromDevice(d *Device) *DeviceState {
	if d == nil {
		return nil
	}
	return &DeviceState{
		DeviceID:   d.DeviceID,
		InstallID:  d.InstallID,
		CDID:       d.CDID,
		Brand:      d.Brand,
		Model:      d.Model,
		OSVersion:  d.OSVersion,
		DPI:        d.DPI,
		Resolution: d.Resolution,
	}
}

// deviceFromState 用已保存的身份重建运行期 Device。
func deviceFromState(st *DeviceState) *Device {
	if st == nil || st.DeviceID == "" {
		return nil
	}
	return &Device{
		DeviceID:   st.DeviceID,
		InstallID:  st.InstallID,
		CDID:       st.CDID,
		Brand:      st.Brand,
		Model:      st.Model,
		OSVersion:  st.OSVersion,
		DPI:        st.DPI,
		Resolution: st.Resolution,
	}
}

// LoadDevice 用已保存的身份替换当前设备，并清空内容密钥。
//
// 密钥与设备绑定，调用方应在之后调用 EnsureKeyPure 重新申请。
func (c *Client) LoadDevice(st *DeviceState) {
	dev := deviceFromState(st)
	if dev == nil {
		return
	}
	c.mu.Lock()
	c.device = dev
	c.key = ""
	c.mu.Unlock()
}

// LoadDeviceFile 从磁盘读取设备身份；文件不存在时返回 (nil, nil)。
func LoadDeviceFile(path string) (*DeviceState, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var st DeviceState
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil, err
	}
	if st.DeviceID == "" {
		return nil, nil
	}
	return &st, nil
}

// SaveDeviceFile 把设备身份写入磁盘（权限 0600，含目录创建）。
func SaveDeviceFile(path string, st *DeviceState) error {
	if path == "" || st == nil || st.DeviceID == "" {
		return nil
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	raw, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0o600)
}
