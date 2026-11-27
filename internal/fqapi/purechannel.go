package fqapi

import (
	"compress/gzip"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// 纯算通道：用本地签名直连内容接口，取回密文再用设备密钥解密。
//
// 这条链路的请求形状来自对照实现（同 aid / version_code=66.9 / Honor10
// 的 App 侧参数 + tt-ok UA）。
// 参数改动会直接改变签名输入，非必要不要动。

const (
	pureUA = "com.dragon.read/66732 (Linux; U; Android 10; zh_CN; Pixel 4 XL; Build/QD1A.190821.007;tt-ok/3.12.13.4-tiktok)"

	pureFullURL  = "https://reading.snssdk.com/reading/reader/full/v/"
	pureBatchURL = "https://api5-normal-sinfonlineb.fqnovel.com/reading/reader/batch_full/v"
)

func pureContentURL(deviceID, itemID string) string {
	q := url.Values{}
	q.Set("aid", aid)
	q.Set("app_name", appName)
	q.Set("channel", "0")
	q.Set("device_platform", devicePlatform)
	q.Set("device_id", deviceID)
	q.Set("device_type", "Honor10")
	q.Set("item_id", itemID)
	q.Set("os_version", "0")
	q.Set("version_code", "66.9")
	return pureFullURL + "?" + q.Encode()
}

func pureBatchContentURL(deviceID, bookID, itemIDs string) string {
	if bookID == "" {
		bookID = "0"
	}
	q := url.Values{}
	q.Set("aid", aid)
	q.Set("app_name", appName)
	q.Set("channel", "0")
	q.Set("device_platform", devicePlatform)
	q.Set("device_id", deviceID)
	q.Set("device_type", "Honor10")
	q.Set("os_version", "0")
	q.Set("version_code", "66.9")
	q.Set("book_id", bookID)
	q.Set("item_ids", itemIDs)
	q.Set("novel_text_type", "1")
	q.Set("req_type", "1")
	return pureBatchURL + "?" + q.Encode()
}

// FetchContentPure 取单章正文（纯算通道）。
//
// 命中 403 / 风控空响应时，如果挂了设备池，会换设备重试（见 withDeviceRotation）。
func (c *Client) FetchContentPure(ctx context.Context, bookID, itemID string) (string, error) {
	var plain string
	err := c.withDeviceRotation(ctx, 3, func() error {
		p, err := c.fetchContentPureOnce(ctx, bookID, itemID)
		if err == nil {
			plain = p
		}
		return err
	})
	if err != nil {
		return "", err
	}
	return plain, nil
}

// fetchContentPureOnce 在单章与批量两个端点之间做一次尝试。
func (c *Client) fetchContentPureOnce(ctx context.Context, bookID, itemID string) (string, error) {
	if err := c.EnsureKeyPure(ctx); err != nil {
		return "", err
	}
	dev := c.Device()
	if dev == nil {
		return "", errors.New("设备未注册")
	}
	key := c.ContentKey()
	if key == "" {
		return "", errors.New("缺少内容密钥")
	}

	// 1) 单章端点
	raw, err := c.pureGet(ctx, pureContentURL(dev.DeviceID, itemID), dev.Cookie())
	if err == nil {
		if enc := contentField(raw, ""); enc != "" {
			if plain, derr := DecryptChapter(enc, key); derr == nil && strings.TrimSpace(plain) != "" {
				return plain, nil
			}
		}
	}
	firstErr := err

	// 2) 批量端点（同一签名方案，参数不同）
	batchURL := pureBatchContentURL(dev.DeviceID, bookID, itemID)
	raw2, err2 := c.pureGet(ctx, batchURL, dev.Cookie())
	if err2 != nil {
		if firstErr != nil {
			return "", fmt.Errorf("单章: %v / 批量: %v", firstErr, err2)
		}
		return "", err2
	}
	if enc := contentField(raw2, itemID); enc != "" {
		if plain, derr := DecryptChapter(enc, key); derr == nil && strings.TrimSpace(plain) != "" {
			return plain, nil
		}
	}
	if firstErr != nil {
		return "", fmt.Errorf("纯算通道无正文: %v", firstErr)
	}
	return "", errors.New("纯算通道返回空正文")
}

func (c *Client) pureGet(ctx context.Context, target, cookie string) ([]byte, error) {
	u, err := url.Parse(target)
	if err != nil {
		return nil, err
	}
	// 签名输入取自实际发送的原始查询串。
	q, _ := url.ParseQuery(u.RawQuery)
	sig := c.signFor(q, cookie, nil)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", pureUA)
	req.Header.Set("Accept-Encoding", "gzip")
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	for k, v := range sig {
		req.Header.Set(k, v)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var body io.Reader = resp.Body
	if resp.Header.Get("Content-Encoding") == "gzip" {
		body = gunzipReader(resp.Body)
	}
	raw, readErr := io.ReadAll(body)
	if readErr != nil {
		return nil, readErr
	}
	if resp.StatusCode >= 400 {
		return nil, &APIError{Status: resp.StatusCode, Body: string(raw), Msg: "内容接口返回错误"}
	}
	if len(raw) == 0 {
		return nil, &APIError{Status: resp.StatusCode, Msg: "网关返回空响应（签名可能未通过风控）"}
	}
	return raw, nil
}

// contentField 从 full（itemID 为空）或 batch 响应里取 content 密文。
func contentField(raw []byte, itemID string) string {
	var root struct {
		Code int             `json:"code"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &root); err != nil {
		return ""
	}
	if itemID == "" {
		var d struct {
			Content string `json:"content"`
		}
		if err := json.Unmarshal(root.Data, &d); err != nil {
			return ""
		}
		return d.Content
	}
	// 批量响应是 {itemID: {content}}，按 id 取本条。
	m := map[string]struct {
		Content string `json:"content"`
	}{}
	if err := json.Unmarshal(root.Data, &m); err != nil {
		return ""
	}
	return m[itemID].Content
}

// EnsureKeyPure 注册设备并申请内容密钥（纯算链路专用）。
//
// 与 EnsureKey 的差别只在 registerkey 请求体的构造：这里按上游 App 的
// 真实形态（device_id 十六进制逐字节反转）来拼，失败时再退回旧构造。
func (c *Client) EnsureKeyPure(ctx context.Context) error {
	if c.Device() != nil && c.ContentKey() != "" {
		return nil
	}
	if err := c.bootstrapPoolDevice(ctx); err != nil {
		return err
	}
	if c.Device() == nil {
		if _, err := c.Register(ctx); err != nil {
			return err
		}
	}
	if c.ContentKey() != "" {
		return nil
	}
	k, err := c.keyForDevice(ctx, c.Device())
	if err != nil {
		return err
	}
	c.SetKey(k)
	return nil
}

// keyForDevice 为指定设备申请内容密钥：先试 App 真实形态，失败再退回旧构造。
func (c *Client) keyForDevice(ctx context.Context, dev *Device) (string, error) {
	if k, err := c.registerKeyPureFor(ctx, dev); err == nil && k != "" {
		return k, nil
	}
	return c.registerKeyFor(ctx, dev)
}

// registerKeyPureFor 用指定设备按 App 真实形态申请内容密钥。
func (c *Client) registerKeyPureFor(ctx context.Context, dev *Device) (string, error) {
	if dev == nil || dev.DeviceID == "" {
		return "", errors.New("设备未注册")
	}
	content, err := registerKeyContentPure(dev.DeviceID)
	if err != nil {
		return "", err
	}
	payload := fmt.Sprintf(`{"content":%q,"keyver":1}`, content)

	q := url.Values{}
	q.Set("aid", aid)
	q.Set("app_name", appName)
	q.Set("channel", "0")
	q.Set("device_id", dev.DeviceID)
	q.Set("device_platform", devicePlatform)
	q.Set("iid", dev.InstallID)
	q.Set("os_version", "0")
	q.Set("version_code", "6.1.5.32")
	q.Set("version_name", "6.1.5.32")

	hdr := map[string]string{
		"User-Agent":      "com.dragon.read",
		"Accept-Encoding": "gzip",
	}
	for k, v := range c.signFor(q, dev.Cookie(), []byte(payload)) {
		hdr[k] = v
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, keyURL+"?"+q.Encode(), strings.NewReader(payload))
	if err != nil {
		return "", err
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var body io.Reader = resp.Body
	if resp.Header.Get("Content-Encoding") == "gzip" {
		body = gunzipReader(resp.Body)
	}
	raw, _ := io.ReadAll(body)
	if len(raw) == 0 {
		return "", &APIError{Status: resp.StatusCode, Msg: "registerkey 空响应"}
	}

	var kr struct {
		Data struct {
			Key string `json:"key"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &kr); err != nil || kr.Data.Key == "" {
		return "", &APIError{Status: resp.StatusCode, Body: string(raw), Msg: "内容密钥申请失败"}
	}
	return decryptContentKey(kr.Data.Key)
}

// registerKeyContentPure 拼 registerkey 的 content 字段。
//
// device_id 十进制 → %032x → 逐字节对反转 → hex2bin(16 字节) 做明文，
// 用固定请求密钥 AES-128-CBC 加密，输出 base64(iv||ct)。
func registerKeyContentPure(deviceID string) (string, error) {
	n, err := strconv.ParseUint(deviceID, 10, 64)
	if err != nil {
		return "", fmt.Errorf("device_id 非数字: %q", deviceID)
	}
	h := fmt.Sprintf("%032x", n)
	reversed := make([]byte, 0, len(h))
	for i := len(h); i >= 2; i -= 2 {
		reversed = append(reversed, h[i-2], h[i-1])
	}
	plain, err := hex.DecodeString(string(reversed))
	if err != nil {
		return "", err
	}
	key, err := base64.StdEncoding.DecodeString(keySecretBase64)
	if err != nil {
		return "", err
	}
	iv := randomBytes(16)
	blk, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	padded := pkcs7(plain, blk.BlockSize())
	ct := make([]byte, len(padded))
	cipher.NewCBCEncrypter(blk, iv).CryptBlocks(ct, padded)
	out := append(append([]byte(nil), iv...), ct...)
	return base64.StdEncoding.EncodeToString(out), nil
}

// signFor 统一签名入口：支持请求体的签名器走 SignBody，其余走 Sign。
func (c *Client) signFor(q url.Values, cookie string, body []byte) map[string]string {
	if len(body) > 0 {
		if bs, ok := c.signer.(BodySigner); ok {
			return bs.SignBody(q, body)
		}
	}
	return c.signer.Sign(q, cookie)
}

// gunzipReader 按需包装 gzip 流；解不开时原样返回响应体。
func gunzipReader(body io.Reader) io.Reader {
	gz, err := gzip.NewReader(body)
	if err != nil {
		return body
	}
	return gz
}
