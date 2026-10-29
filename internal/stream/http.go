package stream

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"healgo-scaffold/internal/response"
)

// 私有键类型避免与业务设置的字符串键冲突，也避免无意覆盖原始取消信号
type requestContextKey struct{}

// PreserveRequestContext 在公共 HTTP 超时生效前保存真正的请求取消信号
// 只有流封装主动取回它；认证和普通 HTTP handler 仍受统一超时约束
func PreserveRequestContext(ctx context.Context) context.Context {
	return context.WithValue(ctx, requestContextKey{}, ctx)
}

type valuesContext struct {
	context.Context
	values context.Context
}

// Deadline/Done/Err 沿用原始请求，Value 沿用当前请求；二者不能一起替换
// 认证等中间件可能在公共超时之后附加值，恢复原始 context 时仍应保留
func (c valuesContext) Value(key any) any { return c.values.Value(key) }

// RequestContext 仅移除公共 HTTP 预算，保留客户端断开信号与后续中间件写入的值
// 不用 context.Background/WithoutCancel 代替整个请求，否则客户端离线后仍会消耗资源
func RequestContext(ctx context.Context) context.Context {
	if ctx == nil {
		return nil
	}
	if original, ok := ctx.Value(requestContextKey{}).(context.Context); ok {
		// WithoutCancel 只用于读取值，屏蔽已舍弃超时的私有取消状态
		// 否则 context.Cause 会误读普通 HTTP 的 DeadlineExceeded，而非真实断线
		return valuesContext{Context: original, values: context.WithoutCancel(ctx)}
	}
	return ctx
}

// Reject 只用于协议开始之前发送事件或升级以后，错误必须通过协议关闭处理
// 复用统一 envelope，但不需要持有 Gin Context，避免后台任务误用已回收的对象
func Reject(w http.ResponseWriter, status int, code, message string) {
	// 错误正文只使用调用方选定的安全分类，不应传入底层驱动或远端错误原文
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(response.Envelope{
		Error:     &response.Error{Code: code, Message: message},
		RequestID: w.Header().Get("X-Request-ID"), TimeStamp: time.Now().UnixMilli(),
	})
}

// ValidateOrigins 使用完整 origin，而不是通配符 host，避免配置意外扩大授权范围
// HTTPS 反向代理的公网 origin 应显式登记；这里不会自行信任 X-Forwarded-*
func ValidateOrigins(origins []string) error {
	for _, origin := range origins {
		if canonicalOrigin(origin) == "" {
			return ErrInvalidOptions
		}
	}
	return nil
}

// OriginAllowed 比较浏览器页面来源的协议、主机和端口，不比较 URL 路径
// 即使来源允许，请求也必须独立通过认证；Origin 不是可信身份
func OriginAllowed(r *http.Request, origins []string) bool {
	values := r.Header.Values("Origin")
	if len(values) == 0 {
		// 非浏览器客户端可以没有 Origin；这不替代身份与权限检查
		return true
	}
	if len(values) != 1 {
		// Origin 按协议只能是一个来源，重复头不能通过选第一个值而绕过检查
		return false
	}
	origin := canonicalOrigin(values[0])
	if origin == "" {
		return false
	}
	scheme := "http"
	// 只由实际 TLS 状态确定同源协议；代理传入的头需要单独的信任边界
	if r.TLS != nil {
		scheme = "https"
	}
	if origin == canonicalOrigin(scheme+"://"+r.Host) {
		return true
	}
	for _, allowed := range origins {
		// 配置在启动时校验；这里仍按完整规范化后的 origin 精确比较
		if origin == canonicalOrigin(allowed) {
			return true
		}
	}
	return false
}

func canonicalOrigin(raw string) string {
	// 只接收完整 origin；路径、凭据和查询串属于 URL 的其他部分，应在配置时纠正
	// 显式检查空 fragment，避免 "https://example.com#" 被解析器悄悄规范化为合法值
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" ||
		u.User != nil || u.Path != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" ||
		strings.ContainsAny(u.Host, "*\\ \t\r\n") || strings.Contains(raw, "#") || strings.HasSuffix(u.Host, ":") {
		return ""
	}
	// 默认端口与显式 80/443 等价，主机名大小写等价，但跨协议不会互相放行
	port := u.Port()
	if port != "" {
		// 端口必须在有效 TCP 范围内，前导零规范化后仍保持相同端口语义
		number, err := strconv.Atoi(port)
		if err != nil || number < 1 || number > 65535 {
			return ""
		}
		port = strconv.Itoa(number)
	} else if u.Scheme == "https" {
		port = "443"
	} else {
		port = "80"
	}
	return u.Scheme + "://" + strings.ToLower(u.Hostname()) + ":" + port
}
