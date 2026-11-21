package fqapi

import (
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"math/big"
	"strings"
	"time"
)

// randomDevice 生成一台随机 Android 设备信息。
type deviceSpec struct {
	DeviceID    string
	InstallID   string
	CDID        string
	ClientUDID  string
	OpenUDID    string
	SigHash     string
	Brand       string
	Model       string
	OSVersion   string
	OSAPI       int
	Release     string
	DPI         string
	Density     string
	Resolution  string
	ROM         string
	CPUABI      string
	IPv6        string
	ReqID       string
	AndroidID   string
	InstallTime int64
}

var brands = []struct {
	name   string
	models []string
}{
	{"Xiaomi", []string{"24031PN0DC", "2304FPN6DC", "23078RKD5C", "M2012K11AC"}},
	{"HUAWEI", []string{"ELS-AN00", "TAS-AL00", "ANA-AN00", "NOH-AN00"}},
	{"OPPO", []string{"CPH2207", "CPH2371", "PEDM00"}},
	{"vivo", []string{"V2197A", "V2073A", "V2055A"}},
	{"OnePlus", []string{"LE2100", "LE2110", "IN2010"}},
	{"samsung", []string{"SM-G9980", "SM-G9910", "SM-N9860"}},
}

var androidVersions = []struct {
	ver     string
	api     int
	release string
}{
	{"12", 32, "V417IR"},
	{"13", 33, "V433IR"},
	{"11", 30, "V394IR"},
}

var resolutions = []struct{ res, dpi, density string }{
	{"2400*1080", "480", "xxhdpi"},
	{"2340*1080", "440", "xxhdpi"},
	{"3200*1440", "640", "xxxhdpi"},
	{"1920*1080", "480", "xxhdpi"},
}

// randomDevice 生成随机设备，未注册的 device_id/install_id 为随机数字串。
func randomDevice() *Device {
	b := brands[randInt(len(brands))]
	model := b.models[randInt(len(b.models))]
	av := androidVersions[randInt(len(androidVersions))]
	rs := resolutions[randInt(len(resolutions))]

	androidID := randHex(16)
	h := fmt.Sprintf("%x", md5.Sum([]byte(androidID)))
	openUDID := h + fmt.Sprintf("%x", md5.Sum([]byte(h)))[:8]

	spec := &deviceSpec{
		DeviceID:    randDigits(16),
		InstallID:   randDigits(16),
		CDID:        randUUID(),
		ClientUDID:  randUUID(),
		OpenUDID:    openUDID,
		SigHash:     randHex(32),
		Brand:       b.name,
		Model:       model,
		OSVersion:   av.ver,
		OSAPI:       av.api,
		Release:     av.release,
		DPI:         rs.dpi,
		Density:     rs.density,
		Resolution:  rs.res,
		ROM:         "1417",
		CPUABI:      "arm64-v8a",
		IPv6:        randIPv6(),
		ReqID:       randUUID(),
		AndroidID:   androidID,
		InstallTime: time.Now().UnixMilli() - randInt64(86400000, 31536000000),
	}

	return &Device{
		DeviceID:   spec.DeviceID,
		InstallID:  spec.InstallID,
		CDID:       spec.CDID,
		Brand:      spec.Brand,
		Model:      spec.Model,
		OSVersion:  spec.OSVersion,
		DPI:        spec.DPI,
		Resolution: spec.Resolution,
		spec:       spec,
	}
}

// header 构造 device_register 的 header 段。
func (d *Device) header(genTime int64) map[string]any {
	s := d.spec
	if s == nil {
		s = &deviceSpec{
			Brand: d.Brand, Model: d.Model, OSVersion: d.OSVersion,
			OSAPI: 33, Release: "V433IR", DPI: d.DPI, Density: "xxhdpi",
			Resolution: d.Resolution, ROM: "1417", CPUABI: "arm64-v8a",
			CDID: d.CDID, OpenUDID: randHex(40), ClientUDID: randUUID(),
			SigHash: randHex(32), IPv6: randIPv6(), ReqID: randUUID(),
			InstallTime: genTime - 86400000,
		}
	}
	return map[string]any{
		"display_name":           "番茄小说",
		"aid":                    mustAtoi64(aid),
		"channel":                "googleplay",
		"package":                "com.dragon.read.oversea.gp",
		"app_version":            "6.8.1.32",
		"version_code":           mustAtoi64(versionCode),
		"update_version_code":    mustAtoi64(updateVersionCd),
		"manifest_version_code":  mustAtoi64(versionCode),
		"app_version_minor":      "6.8.1.32",
		"sdk_version":            "3.7.0-rc.25-fanqie-xiaoshuo-opt",
		"sdk_target_version":     29,
		"git_hash":               "5b6a0d3",
		"sdk_flavor":             "china",
		"guest_mode":             0,
		"is_system_app":          0,
		"pre_installed_channel":  "",
		"not_request_sender":     0,
		"os":                     "Android",
		"os_version":             s.OSVersion,
		"os_api":                 s.OSAPI,
		"device_model":           s.Model,
		"device_brand":           s.Brand,
		"device_manufacturer":    s.Brand,
		"cpu_abi":                s.CPUABI,
		"release_build":          s.Release + "_20171120",
		"density_dpi":            mustAtoi64(s.DPI),
		"display_density":        s.Density,
		"resolution":             strings.ReplaceAll(s.Resolution, "*", "x"),
		"language":               "zh",
		"timezone":               8,
		"access":                 "wifi",
		"rom":                    s.ROM,
		"rom_version":            s.Release + " release-keys",
		"cdid":                   s.CDID,
		"sig_hash":               s.SigHash,
		"openudid":               s.OpenUDID,
		"clientudid":             s.ClientUDID,
		"ipv6_list":              []map[string]string{{"type": "client_anpi", "value": s.IPv6}},
		"region":                 "CN",
		"tz_name":                "Asia/Shanghai",
		"tz_offset":              28800,
		"sim_serial_number":      []any{},
		"oaid_may_support":       false,
		"req_id":                 s.ReqID,
		"device_platform":        "android",
		"custom":                 map[string]any{"host_bit": 64, "account_region": "cn", "dragon_device_type": "phone"},
		"apk_first_install_time": s.InstallTime,
	}
}

// ---- 随机工具 ----

func randInt(n int) int {
	if n <= 0 {
		return 0
	}
	v, err := rand.Int(rand.Reader, big.NewInt(int64(n)))
	if err != nil {
		return 0
	}
	return int(v.Int64())
}

func randInt64(min, max int64) int64 {
	if max <= min {
		return min
	}
	v, err := rand.Int(rand.Reader, big.NewInt(max-min))
	if err != nil {
		return min
	}
	return min + v.Int64()
}

func randHex(n int) string {
	b := make([]byte, (n+1)/2)
	if _, err := rand.Read(b); err != nil {
		return strings.Repeat("0", n)
	}
	return hex.EncodeToString(b)[:n]
}

func randDigits(n int) string {
	var sb strings.Builder
	sb.Grow(n)
	for range n {
		sb.WriteByte(byte('0' + randInt(10)))
	}
	return sb.String()
}

func randUUID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "00000000-0000-4000-8000-000000000000"
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b)
	return fmt.Sprintf("%s-%s-%s-%s-%s", h[0:8], h[8:12], h[12:16], h[16:20], h[20:32])
}

func randIPv6() string {
	parts := make([]string, 8)
	for i := range parts {
		parts[i] = fmt.Sprintf("%04X", randInt(65536))
	}
	return strings.Join(parts, ":")
}
