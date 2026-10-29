// The executable is deliberately thin: pkg/wire constructs the dependencies,
// while pkg/bootstrap prepares schema and owns the running server lifecycle.
package cmd

import (
	"context"
	"errors"
	"flag"

	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"healgo-scaffold/configs"
	"healgo-scaffold/pkg/bootstrap"
	"healgo-scaffold/pkg/router"
	"healgo-scaffold/pkg/wire"
)

// Run 是全部可执行入口共享的启动流程，enableExamples 决定是否登记演示表和路由
func Run(args []string, enableExamples bool) error {
	// 无子命令默认启动服务；显式子命令让容器初始化和探活复用同一二进制
	// 命令解析只做入口选择，实际依赖组装由 pkg/wire 统一维护
	command := "serve"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		command, args = args[0], args[1:]
	}
	if command != "serve" && command != "migrate" && command != "healthcheck" {
		return errors.New("usage: server [serve|migrate|healthcheck] -config <file> [-di wire|manual] [-addr host:port]")
	}
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	// 标准 flag 可能把误输入的参数原文打印出来，因此使用静态用法提示
	// 密码只写入私有文件，不通过命令行参数传递，避免进程列表泄露
	flags.SetOutput(io.Discard)
	path := flags.String("config", "configs/config.local.yaml", "bootstrap YAML path")
	// 切换注入方式只改变对象组装，配置、模型初始化和 HTTP 协议保持相同
	di := flags.String("di", "wire", "dependency injection: wire or manual")
	addr := flags.String("addr", "", "optional HTTP address override")
	if flags.Parse(args) != nil || flags.NArg() != 0 || (*di != "manual" && *di != "wire") {
		return errors.New("usage: server [serve|migrate|healthcheck] -config <file> [-di wire|manual] [-addr host:port]")
	}
	cfg, err := configs.Load(*path)
	if err != nil {
		return err
	}
	// 示例入口默认使用独立端口，也允许通过 -addr 显式覆盖
	if *addr != "" {
		cfg.HTTP.Addr = *addr
	} else if enableExamples {
		cfg.HTTP.Addr = "127.0.0.1:18082"
	}
	// 探活不创建 DB 连接池或内存会话，否则频繁检查会产生多余资源
	// /readyz 由正在运行的主进程检查其已有连接池，结果才代表服务状态
	if command == "healthcheck" {
		return healthcheck(cfg.HTTP.Addr)
	}
	// 启动过程中也使用此 context，容器停止能取消连接重试和 schema 操作
	// Docker stop 发 SIGTERM，本机 Ctrl+C 发中断；两者走同一优雅停机路径
	// 注销信号监听放在 defer，退出时归还 NotifyContext 持有的资源
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// 默认使用 Wire 生成的显式装配；手写入口可用来学习或手动维护依赖
	// 容器构造成功意味着模型和种子已准备好，路由只负责协议层登记
	constructor := wire.NewContainer
	if *di == "manual" {
		constructor = wire.NewContainerManual
	}
	container, err := constructor(ctx, cfg, bootstrap.Options{InitializeSchema: command == "migrate", EnableExamples: enableExamples})
	if err != nil {
		return err
	}
	// 任何后续启动错误都能关闭容器；成功监听后 Runtime 会先排空再关闭
	defer container.Close()
	// 路由登记失败也属于启动失败，此时尚未监听，直接关闭已构造资源即可
	engine, err := router.NewEngine(container)
	if err != nil {
		return err
	}
	// 引擎初始化与模块登记分开，业务路由统一从 router.New 显式接入
	if err := router.New(engine, container); err != nil {
		return err
	}
	runtime := bootstrap.NewRuntime(bootstrap.Components{Config: container.Config, Resources: container.Resources, Router: engine}, container.Close)
	defer runtime.Close()
	// migrate 只执行初始化，不启动监听；退出码 0 供 Compose 判断依赖完成
	// 多次运行补缺失数据，不将初始密码重新覆盖到现有账号
	if command == "migrate" {
		runtime.Logger.Info("schema_and_system_seeds_initialized")
		return nil
	}
	return runtime.Serve(ctx)
}

// Container health checks use the same saved port as the server, but contact
// loopback when it listens on a wildcard interface. They never open another DB.
func healthcheck(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return errors.New("invalid healthcheck address")
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	target := url.URL{Scheme: "http", Host: net.JoinHostPort(host, port), Path: "/readyz"}
	client := &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{Proxy: nil}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	// 显式关闭代理和重定向，探活只验证配置指定的本机服务
	// 限制读取量并关闭 body，避免异常端点使检查占用无限内存或连接
	defer client.CloseIdleConnections()
	resp, err := client.Get(target.String())
	if err != nil {
		return errors.New("service is not ready")
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK {
		return errors.New("service is not ready")
	}
	return nil
}
