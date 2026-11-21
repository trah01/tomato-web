package main

import (
	_ "embed"
	"fmt"
)

//go:embed static/home.html
var homeHTML []byte

// homePage 返回首页 HTML。
func homePage() []byte {
	if len(homeHTML) == 0 {
		return []byte(fmt.Sprintf("<html><body>%s</body></html>", "首页未构建"))
	}
	return homeHTML
}
