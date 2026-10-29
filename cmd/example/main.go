// 示例入口：显式启用 Note CRUD、WebSocket 与 SSE 路由
package main

import (
	"fmt"
	"heago-scaffold/cmd"
	"os"
)

func main() {
	// true 让模型登记和路由挂载同时启用示例，避免只开接口却没建表
	// 只输出已经脱敏的启动错误，运行流程统一放在 cmd.Run
	if err := cmd.Run(os.Args[1:], true); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
