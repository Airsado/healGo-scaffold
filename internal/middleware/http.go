// Package middleware applies cross-cutting HTTP behavior before module routes run.
package middleware

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"heago-scaffold/internal/response"
	"heago-scaffold/internal/stream"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// RequestID 在处理链最前面创建关联标识，后续日志和错误响应都使用同一个值
// 它是排障线索，不是访问凭证，因此不能用来代替身份认证或请求签名
func RequestID() gin.HandlerFunc {
	return func(c *gin.Context) {
		// Generate our own ID: arbitrary incoming identifiers must not become log content.
		id := uuid.NewString()
		// Gin context 只在当前请求中传递；响应头便于前端提交精确的故障线索
		c.Set(response.RequestIDKey, id)
		c.Header("X-Request-ID", id)
		c.Next()
	}
}

// 访问日志包裹其后的处理链，才能同时记录认证拒绝、处理成功和恢复后的 panic
// 耗时在 c.Next 返回后计算，包含下游中间件与控制器消耗的总时间
func AccessLog(log *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		// Registered paths contain parameter names; raw paths and queries can contain secrets.
		// Body, headers, panic values and credential-bearing database errors are never logged.
		log.Info("http_request", "method", c.Request.Method, "route", c.FullPath(), "status", c.Writer.Status(), "duration_ms", time.Since(start).Milliseconds(), "request_id", c.GetString(response.RequestIDKey))
	}
}

func Recovery(log *slog.Logger) gin.HandlerFunc {
	// recovery 只处理当前请求 goroutine 的 panic，业务自行创建的 goroutine 需自行管理生命周期
	// 错误响应保留 requestId，原始 panic 值即使包含秘密也不会进入日志和响应
	// Gin's default recovery prints panic values and request headers; suppress that writer.
	return gin.CustomRecoveryWithWriter(nil, func(c *gin.Context, _ any) {
		log.Error("http_panic", "route", c.FullPath(), "request_id", c.GetString(response.RequestIDKey))
		// SSE 已开始或 WebSocket 已升级时不能再拼接 JSON，协议封装负责在栈展开时清理
		// 普通 HTTP 尚未提交响应的 panic 仍返回原来的统一 500
		if c.Writer.Written() {
			c.Abort()
			return
		}
		response.Fail(c, 500, "internal_error", "服务器内部错误")
	})
}

// Limits 必须放在正文解析之前；解析后再检查长度已经无法追回分配的内存
// HTTP 服务器的读写超时保护连接，本中间件的 context 预算约束实际业务工作
func Limits(maxBody int64, timeout time.Duration) gin.HandlerFunc {
	return func(c *gin.Context) {
		// 通用 API 默认不缓存响应，避免账号、配置等内容停留在共享缓存中
		c.Header("X-Content-Type-Options", "nosniff")
		c.Header("Referrer-Policy", "no-referrer")
		c.Header("Cache-Control", "no-store")
		if c.Request.ContentLength > maxBody {
			// 已知长度可提前拒绝，避免大请求先进入 JSON 解码器
			response.Fail(c, 413, "body_too_large", "请求体超出大小限制")
			return
		}
		// 分块传输可能没有 Content-Length，仍需限制实际读取的字节数
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxBody)
		// 显式流封装可以使用原始取消信号另设时长；认证与普通接口仍受本预算限制
		parent := stream.PreserveRequestContext(c.Request.Context())
		ctx, cancel := context.WithTimeout(parent, timeout)
		// 即使处理很快也要取消子 context，及时释放超时计时器占用的资源
		defer cancel()
		// A context is cooperative cancellation; module queries and clients must use it.
		// We avoid timeout goroutines that could write a response after Gin reused its context.
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}
}

// CORS 处理浏览器来源限制；命令行客户端仍必须独立通过 token 和权限校验
// 启动时复制精确白名单，避免外部修改切片导致运行中的信任范围变化
func CORS(origins []string) gin.HandlerFunc {
	origins = append([]string(nil), origins...)
	return func(c *gin.Context) {
		origin := c.GetHeader("Origin")
		if origin == "" {
			// 同源或非浏览器请求可能没有 Origin，继续交给正常的认证与路由链处理
			c.Next()
			return
		}
		// 提醒代理不同 Origin 的访问结果可能不同，不能直接复用另一来源的响应
		c.Header("Vary", "Origin")
		// WebSocket 浏览器握手总会带 Origin，同源请求无需另行登记跨域白名单
		if !stream.OriginAllowed(c.Request, origins) {
			response.Fail(c, 403, "origin_forbidden", "该浏览器来源未获允许")
			return
		}
		c.Header("Access-Control-Allow-Origin", origin)
		// 浏览器默认不能读取全部响应头，显式开放排障用的请求标识
		c.Header("Access-Control-Expose-Headers", "X-Request-ID")
		if c.Request.Method == http.MethodOptions {
			// 预检只回答允许的协议能力，不应执行登录、写库或其他真实业务动作
			// Only the bearer/header API surface is allowed; cookie credentials are not enabled.
			method := c.GetHeader("Access-Control-Request-Method")
			if !strings.Contains(",GET,POST,PUT,PATCH,DELETE,OPTIONS,", ","+method+",") || method == "" {
				response.Fail(c, 403, "cors_method_forbidden", "跨域请求方法不受支持")
				return
			}
			// 请求头名称大小写不敏感，先规范化再与白名单比较
			for _, header := range strings.Split(c.GetHeader("Access-Control-Request-Headers"), ",") {
				header = strings.ToLower(strings.TrimSpace(header))
				if header != "" && header != "authorization" && header != "content-type" {
					response.Fail(c, 403, "cors_header_forbidden", "跨域请求头不受支持")
					return
				}
			}
			c.Header("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
			c.Header("Access-Control-Allow-Headers", "Authorization, Content-Type")
			// 适度缓存成功预检，减少浏览器额外往返，同时让策略调整能够较快生效
			c.Header("Access-Control-Max-Age", "600")
			// 204 没有正文；终止链条防止预检请求进入实际接口
			c.AbortWithStatus(204)
			return
		}
		c.Next()
	}
}

// LoginBudget has bounded state per process, not an unbounded attacker-controlled IP map.
// It protects password hashing capacity; it is intentionally independent of session checks.
func LoginBudget(max int) gin.HandlerFunc {
	// 闭包状态由本次路由注册共享，而非每次请求新建；否则限流计数永远无法累积
	// 这是整个进程的容量保护，不承诺按用户/IP 公平，也不替代网关的分布式限流
	var mu sync.Mutex
	var window time.Time
	var count int
	return func(c *gin.Context) {
		// 并发请求必须原子地更新窗口与计数，避免多个 goroutine 同时放行超额尝试
		mu.Lock()
		if time.Since(window) >= time.Minute {
			window = time.Now()
			count = 0
		}
		count++
		limited := count > max
		// 解锁后再写 HTTP 响应，避免慢客户端阻塞其他请求更新限流状态
		mu.Unlock()
		if limited {
			// 429 让客户端区分临时限流与密码错误，并用 Retry-After 提示重试节奏
			c.Header("Retry-After", "60")
			response.Fail(c, 429, "login_rate_limited", "登录尝试过多，请稍后重试")
			return
		}
		c.Next()
	}
}
