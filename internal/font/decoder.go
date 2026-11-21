package font

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// Decoder 把番茄字体混淆的私用区字符（U+E000–U+F8FF）还原成真实汉字。
//
// 番茄的做法：把正文里一批字符替换成 PUA 码位，再下发一个把 PUA 码位
// 映射到「错误字形」的 woff2 字体。浏览器按字形渲染，人眼看到的是正确
// 汉字，但文本层是乱码。
//
// 还原原理：把混淆字体的每个 glyph 轮廓与标准思源黑体做哈希比对，
// 找出「这个 PUA 码位的字形实际上是哪个汉字」，得到 PUA→汉字 映射表。
//
// 映射表由 BuildMap 从字体文件生成，或直接加载 benchmark/map.json。
type Decoder struct {
	mu  sync.RWMutex
	tbl map[rune]rune
}

// New 创建空解码器。
func New() *Decoder {
	return &Decoder{tbl: make(map[rune]rune)}
}

// Load 从 JSON 映射文件加载（格式：{"\ue4aa":"哪", ...}）。
func (d *Decoder) Load(path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	tbl, err := parseMapFile(raw)
	if err != nil {
		return err
	}
	d.mu.Lock()
	d.tbl = tbl
	d.mu.Unlock()
	return nil
}

// SetMap 直接替换映射表。
func (d *Decoder) SetMap(tbl map[rune]rune) {
	d.mu.Lock()
	d.tbl = tbl
	d.mu.Unlock()
}

// Size 返回映射表大小。
func (d *Decoder) Size() int {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return len(d.tbl)
}

// Decode 还原字符串中的 PUA 字符。
func (d *Decoder) Decode(s string) string {
	d.mu.RLock()
	tbl := d.tbl
	d.mu.RUnlock()
	if len(tbl) == 0 {
		return s
	}
	out := make([]rune, 0, len(s))
	for _, r := range s {
		if rep, ok := tbl[r]; ok {
			out = append(out, rep)
			continue
		}
		out = append(out, r)
	}
	return string(out)
}

// HasPUA 判断字符串是否含未还原的 PUA 字符。
func HasPUA(s string) bool {
	for _, r := range s {
		if r >= 0xE000 && r <= 0xF8FF {
			return true
		}
	}
	return false
}

// AutoLoad 尝试从若干候选路径加载映射表，返回实际加载的路径。
func (d *Decoder) AutoLoad(candidates ...string) (string, error) {
	for _, p := range candidates {
		if p == "" {
			continue
		}
		if _, err := os.Stat(p); err != nil {
			continue
		}
		if err := d.Load(p); err != nil {
			return "", fmt.Errorf("加载 %s: %w", p, err)
		}
		return p, nil
	}
	return "", errors.New("未找到字体映射表")
}

// ResolvePath 在若干目录下查找文件名。
func ResolvePath(name string, dirs ...string) string {
	for _, dir := range dirs {
		if dir == "" {
			continue
		}
		p := filepath.Join(dir, name)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}
