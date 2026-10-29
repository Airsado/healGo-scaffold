// 模板根入口：go run .；详细启动流程见 cmd/gin_server.go
package main

import (
	"fmt"
	"heago-scaffold/cmd"
	"os"
)

func main() {
	// false 选择通用模板；教学路由由 cmd/example 显式开启
	// 只输出已经脱敏的启动错误，运行流程统一放在 cmd.Run
	if err := cmd.Run(os.Args[1:], false); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
