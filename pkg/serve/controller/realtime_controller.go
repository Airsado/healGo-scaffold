package controller

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"time"

	"healgo-scaffold/internal/auth"
	"healgo-scaffold/internal/response"
	"healgo-scaffold/internal/stream"
	"healgo-scaffold/internal/stream/sse"
	streamws "healgo-scaffold/internal/stream/websocket"

	"github.com/gin-gonic/gin"
)

// RealtimePermission 由路由与连接内授权共用，避免控制器反向依赖路由包形成循环
const RealtimePermission = "example:realtime:connect"

// RealtimeController only demonstrates transport behavior. It owns neither a database
// pool nor background workers; the common stream manager owns active sessions.
type RealtimeController struct {
	authentication *auth.Service
	streams        *stream.Manager
	tickets        *ticketStore
	// 来源白名单属于本次应用实例的配置，不在请求中从转发头推导
	origins []string
}

// NewRealtimeController 将私有 ticket 存储封装在控制器内，调用方只注入共享基础设施
// 不建立额外认证实例、不启动后台任务；连接仍由宿主的 Manager 管理
func NewRealtimeController(authentication *auth.Service, streams *stream.Manager, origins []string) (*RealtimeController, error) {
	if authentication == nil || streams == nil {
		return nil, errors.New("realtime dependencies are required")
	}
	// ticket 存储是有容量与时效边界的内存数据，不创建数据库表或清理 goroutine
	// 复制 origins，避免外部修改切片时改变正在运行实例的来源策略
	return &RealtimeController{authentication: authentication, streams: streams, tickets: newTicketStore(), origins: append([]string(nil), origins...)}, nil
}

// authorizer holds a standalone sanitized HTTP request, never a Gin context.
// Checking permissions alone would miss a logged-out or naturally expired token,
// so every invocation first authenticates the original bearer session again.
func (h *RealtimeController) authorizer(request *http.Request, token string) func(context.Context) error {
	base := request.Clone(context.Background())
	// Rebuild headers rather than sharing a request's mutable map. Drop the
	// ticket query and body too: later checks need only the bearer credential.
	base.Header = make(http.Header)
	base.Header.Set("Authorization", "Bearer "+token)
	base.URL = &url.URL{Path: request.URL.Path}
	base.RequestURI = ""
	base.Body = http.NoBody
	base.ContentLength = 0
	return func(ctx context.Context) error {
		principal, _, err := h.authentication.Authenticate(ctx, base.Clone(ctx))
		if err != nil {
			return err
		}
		return h.authentication.RequirePermission(ctx, principal, RealtimePermission)
	}
}

func (h *RealtimeController) authenticate(request *http.Request) (string, error) {
	// This synchronous check runs within the ordinary HTTP request budget.
	// Serve performs its own initial check again to close the revocation race.
	principal, token, err := h.authentication.Authenticate(request.Context(), request)
	if err != nil {
		return "", err
	}
	if err := h.authentication.RequirePermission(request.Context(), principal, RealtimePermission); err != nil {
		return "", err
	}
	return token, nil
}

func (h *RealtimeController) Ticket(c *gin.Context) {
	// 只有签发普通 bearer 所属会话的短期凭据，不为 ticket 单独授予角色或权限
	// Issuing a browser credential is itself privileged. Revalidation also
	// handles logout racing with the protected group's earlier authentication.
	token, err := h.authenticate(c.Request)
	if err != nil {
		rejectAuth(c, err)
		return
	}
	// 复查身份成功后才签发，避免用已经登出的会话继续兑换连接凭据
	ticket, err := h.tickets.issue(token, time.Now())
	if err != nil {
		if errors.Is(err, errTicketCapacity) {
			c.Header("Retry-After", "30")
			response.Fail(c, 429, "realtime_ticket_capacity", "连接凭据已达容量，请稍后重试")
		} else {
			response.Fail(c, 503, "realtime_ticket_unavailable", "连接凭据暂不可用")
		}
		return
	}
	// Cache-Control is supplied by framework middleware. Tickets never appear
	// in logs or persistence, and are not valid for any ordinary HTTP API.
	response.Write(c, http.StatusCreated, gin.H{"ticket": ticket, "expires_in": int(ticketTTL / time.Second)})
}

func (h *RealtimeController) SSE(c *gin.Context) {
	// Build authorization per connection: periodic checks must refer to this
	// user's session rather than a principal cached at module construction.
	token, err := h.authenticate(c.Request)
	if err != nil {
		rejectAuth(c, err)
		return
	}
	server, err := sse.New(h.streams, sse.Options{Authorize: h.authorizer(c.Request, token)})
	if err != nil {
		response.Fail(c, 503, "realtime_unavailable", "实时连接暂不可用")
		return
	}
	// Serve writes its own safe initialization errors. Once streaming starts,
	// returning ends the stream; writing a JSON envelope would corrupt framing.
	_ = server.Serve(c.Writer, c.Request, func(ctx context.Context, out *sse.Writer) error {
		// 使用流封装恢复后的 context；普通 HTTP 短超时不能限制整个 SSE 会话
		ticker := time.NewTicker(time.Second)
		// A timer is stopped with the callback; this example has no independent
		// publishing goroutine that could keep sending after the client leaves.
		defer ticker.Stop()
		for sequence := 1; sequence <= 5; sequence++ {
			if err := out.SendJSON("tick", struct {
				Sequence int `json:"sequence"`
			}{sequence}); err != nil {
				return err
			}
			if sequence < 5 {
				select {
				case <-ctx.Done():
					// 每个等待点响应取消，停机才能等到回调真正结束再释放资源
					return ctx.Err()
				case <-ticker.C:
				}
			}
		}
		return out.SendJSON("done", struct {
			Count int `json:"count"`
		}{5})
	})
}

func (h *RealtimeController) WebSocket(c *gin.Context) {
	// Reject token query parameters explicitly. A ticket is a narrow, short
	// handshake credential, not permission to accept ordinary URL bearer tokens.
	query, err := url.ParseQuery(c.Request.URL.RawQuery)
	if err != nil || len(query) > 1 || (len(query) == 1 && !query.Has("ticket")) {
		response.Fail(c, 400, "realtime_invalid_ticket", "连接参数不正确")
		return
	}
	var token string
	if values, exists := query["ticket"]; exists {
		// Credentials cannot be combined or repeated. Native browser WebSocket
		// uses this single ticket; CLI clients use the ordinary bearer header.
		if len(values) != 1 || len(c.Request.Header.Values("Authorization")) != 0 {
			response.Fail(c, 400, "realtime_invalid_ticket", "连接参数不正确")
			return
		}
		token, err = h.tickets.consume(values[0], time.Now())
		if err != nil {
			response.Fail(c, 401, "realtime_ticket_invalid", "连接凭据无效或已过期")
			return
		}
		// 单次消费只证明曾签发该 ticket，仍须核对原会话及当前权限
		err = h.authorizer(c.Request, token)(c.Request.Context())
	} else {
		token, err = h.authenticate(c.Request)
	}
	if err != nil {
		rejectAuth(c, err)
		return
	}
	server, err := streamws.New(h.streams, streamws.Options{AllowedOrigins: h.origins, Authorize: h.authorizer(c.Request, token)})
	if err != nil {
		response.Fail(c, 503, "realtime_unavailable", "实时连接暂不可用")
		return
	}
	// A read loop is required even for a push service, because the WebSocket
	// library processes ping/pong/close control frames while reading messages.
	_ = server.Serve(c.Writer, c.Request, func(ctx context.Context, connection *streamws.Conn) error {
		// 同步回显直接感知网络背压，不在控制器积累无界发送队列
		for {
			kind, payload, err := connection.Read(ctx)
			if err != nil {
				return err
			}
			if err := connection.Write(ctx, kind, payload); err != nil {
				return err
			}
		}
	})
}

func rejectAuth(c *gin.Context, err error) {
	// 认证不可用与身份无效分开，防止客户端因短暂故障清空仍有效的登录态
	// No raw session error crosses the HTTP boundary: token material may be
	// carried by a storage adapter even when the operation is only a lookup.
	switch {
	case errors.Is(err, auth.ErrUnavailable):
		response.Fail(c, 503, "auth_unavailable", "认证服务暂不可用")
	case errors.Is(err, auth.ErrForbidden):
		response.Fail(c, 403, "forbidden", "没有实时连接权限")
	default:
		response.Fail(c, 401, "unauthorized", "请重新登录")
	}
}
