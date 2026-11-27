package fqapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// 设备阅读权益（privilege/add）。
//
// 设备注册之后，App 会再补一次「加权益」请求，给这台设备挂 30 天阅读权益
// （amount=2592000 秒）。上游把这一步当作可选环节：失败只影响部分章节的
// 可读范围，不影响设备本身是否可用，所以这里也按「尽力而为」处理。
//
// 请求形状对齐上游 device.ts 的 activatePremium：
//
//	POST https://api5-normal-sinfonlinea.fqnovel.com/reading/user/privilege/add/v?<query>
//	{"add_count_daily":0,"amount":2592000,"privilege_id":7210376203117531962,
//	 "from":8,"unique_key":"<毫秒时间戳>"}
//
// 注意 privilege_id 超出 float64 安全整数范围，必须拼进原始 JSON 字符串，
// 不能先转成 float 再序列化（那样会变成 ...2000）。
const (
	privilegeVersionCode = "63932"
	privilegeVersionName = "6.3.9.32"
	privilegeID          = "7210376203117531962"
	privilegeAmount      = 2592000 // 30 天，单位秒
)

// privilegeAddURL 是权益接口地址；测试里可替换。
var privilegeAddURL = "https://api5-normal-sinfonlinea.fqnovel.com/reading/user/privilege/add/v"

// ActivatePremium 给指定设备加 30 天阅读权益，返回过期时间（Unix 秒，未知时为 0）。
//
// 该接口是可选步骤：调用方拿到错误时可以忽略，设备仍能取正文。
func (c *Client) ActivatePremium(ctx context.Context, st *DeviceState) (int64, error) {
	if st == nil || st.DeviceID == "" {
		return 0, fmt.Errorf("设备未注册")
	}

	q := url.Values{}
	q.Set("aid", aid)
	q.Set("app_name", appName)
	q.Set("channel", "0")
	q.Set("device_id", st.DeviceID)
	q.Set("device_platform", devicePlatform)
	q.Set("device_type", firstNonEmptyStr(st.Model, "Pixel 4 XL"))
	q.Set("iid", st.InstallID)
	q.Set("os_version", "0")
	q.Set("version_code", privilegeVersionCode)
	q.Set("version_name", privilegeVersionName)
	q.Set("manifest_version_code", privilegeVersionCode)
	q.Set("update_version_code", privilegeVersionCode)

	// privilege_id 必须原样拼进 JSON，不能走 json.Marshal。
	payload := fmt.Sprintf(
		`{"add_count_daily":0,"amount":%d,"privilege_id":%s,"from":8,"unique_key":"%d"}`,
		privilegeAmount, privilegeID, time.Now().UnixMilli(),
	)

	hdr := map[string]string{
		"User-Agent":      "com.dragon.read",
		"Accept-Encoding": "gzip",
		"Content-Type":    "application/json; charset=utf-8",
	}
	for k, v := range c.signFor(q, "", []byte(payload)) {
		hdr[k] = v
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, privilegeAddURL+"?"+q.Encode(), strings.NewReader(payload))
	if err != nil {
		return 0, err
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	var body io.Reader = resp.Body
	if resp.Header.Get("Content-Encoding") == "gzip" {
		body = gunzipReader(resp.Body)
	}
	raw, _ := io.ReadAll(body)
	if resp.StatusCode >= 400 {
		return 0, &APIError{Status: resp.StatusCode, Body: string(raw), Msg: "权益接口返回错误"}
	}
	if len(raw) == 0 {
		return 0, &APIError{Status: resp.StatusCode, Msg: "权益接口返回空响应"}
	}

	var jr struct {
		Code int `json:"code"`
		Data struct {
			ExpireTime   json.Number `json:"expire_time"`
			VIPExpire    json.Number `json:"vip_expire_time"`
			PrivilegeEnd json.Number `json:"privilege_end_time"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &jr); err != nil {
		return 0, &APIError{Status: resp.StatusCode, Body: string(raw), Msg: "权益响应解析失败"}
	}
	if jr.Code != 0 {
		return 0, &APIError{Status: resp.StatusCode, Body: string(raw), Msg: fmt.Sprintf("权益接口业务错误 code=%d", jr.Code)}
	}

	for _, n := range []json.Number{jr.Data.ExpireTime, jr.Data.VIPExpire, jr.Data.PrivilegeEnd} {
		if v, err := strconv.ParseInt(n.String(), 10, 64); err == nil && v > 0 {
			// 上游有的字段是秒，有的带毫秒；统一成秒。
			if v > 1e12 {
				v /= 1000
			}
			return v, nil
		}
	}
	return 0, nil
}
