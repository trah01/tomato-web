package font

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
)

// parseMapFile 解析 {"\ue4aa":"哪"} 形式的映射表。
func parseMapFile(raw []byte) (map[rune]rune, error) {
	var strMap map[string]string
	if err := json.Unmarshal(raw, &strMap); err != nil {
		return nil, fmt.Errorf("映射表 JSON 非法: %w", err)
	}
	out := make(map[rune]rune, len(strMap))
	for k, v := range strMap {
		kr := []rune(k)
		vr := []rune(v)
		if len(kr) != 1 || len(vr) != 1 {
			continue
		}
		out[kr[0]] = vr[0]
	}
	return out, nil
}

// ExportMap 把映射表写成 JSON（便于人工核对）。
func ExportMap(tbl map[rune]rune, path string) error {
	strMap := make(map[string]string, len(tbl))
	keys := make([]rune, 0, len(tbl))
	for k := range tbl {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	for _, k := range keys {
		strMap[string(k)] = string(tbl[k])
	}
	raw, err := json.MarshalIndent(strMap, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0o644)
}

// Stats 返回映射表的可读统计。
func Stats(tbl map[rune]rune) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "映射条目: %d\n", len(tbl))
	sample := 0
	for k, v := range tbl {
		if sample >= 20 {
			break
		}
		fmt.Fprintf(&sb, "  U+%04X -> %c\n", k, v)
		sample++
	}
	return sb.String()
}
