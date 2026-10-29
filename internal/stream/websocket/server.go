// Package websocket 为显式选择的长连接提供容量、授权和取消边界
// 它不包含广播、订阅队列或消息持久化，应用仍须管理自己的业务 goroutine
package websocket

import (
	"context"
	"errors"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"

	"healgo-scaffold/internal/stream"

	coder "github.com/coder/websocket"
)

var (
	// 固定错误分类供 errors.Is 判断；不能把远端可控的 reason 包进返回值
	// 身份认证仍由应用的 Authorize 回调决定，本包只控制检查时机与关闭行为
	ErrInvalidOptions  = stream.ErrInvalidOptions
	ErrClosed          = errors.New("websocket connection closed")
	ErrHandshake       = errors.New("websocket handshake failed")
	ErrUnauthorized    = errors.New("websocket authorization failed")
	ErrOriginForbidden = errors.New("websocket origin forbidden")
	ErrInvalidMessage  = errors.New("invalid websocket message")
	ErrMessageTooBig   = errors.New("websocket message exceeds limit")
	ErrUnavailable     = errors.New("websocket transport unavailable")
	ErrHandler         = errors.New("websocket handler failed")
)

// Options 的零值选择有限默认值；负数不能用来关闭限额或心跳
// Authorize 应读取当前会话与权限，并且必须协作响应传入 context 的取消
type Options struct {
	// MaxDuration 限制整个回调的生命周期；达到期限会取消 context 并关闭 socket
	// WriteTimeout 也覆盖等待并发写锁的时间，并给授权查询分配相同的预算
	MaxDuration  time.Duration
	WriteTimeout time.Duration
	// PingInterval 决定何时发起活性检查；PongTimeout 是单次检查的最长等待
	// 客户端与服务端都必须持续读取，协议库才能处理 ping/pong 控制帧
	PingInterval time.Duration
	PongTimeout  time.Duration
	// RecheckInterval 重新读取当前授权状态，不应只检查连接建立时缓存的角色
	// 检查与心跳串行执行，单次检查期间不会额外堆积新的检查 goroutine
	RecheckInterval time.Duration
	// MaxMessageBytes 同时约束完整读消息与写消息，默认禁用压缩以降低资源变化
	// AllowedOrigins 必须是 http(s) 的完整来源；空列表仍允许同源与无 Origin 请求
	MaxMessageBytes int64
	AllowedOrigins  []string
	// Authorize 的 nil 值代表本层不做权限查询，调用者必须自行完成接入认证
	// 回调需要响应 context；本包不会用丢弃 goroutine 的方式伪造超时完成
	Authorize func(context.Context) error
}

// Server 构造后不可变，同一实例可处理并发请求；Manager 在协议之间共享容量
type Server struct {
	manager *stream.Manager
	opts    Options
}

// New 不启动 goroutine；只有成功升级的连接才需要心跳和取消监视任务
// 同一个 Manager 可以同时登记 SSE 与 WebSocket，防止每个协议各自突破总容量
func New(manager *stream.Manager, opts Options) (*Server, error) {
	if manager == nil || opts.MaxDuration < 0 || opts.WriteTimeout < 0 || opts.PingInterval < 0 || opts.PongTimeout < 0 || opts.RecheckInterval < 0 || opts.MaxMessageBytes < 0 || opts.MaxMessageBytes == math.MaxInt64 {
		// 库的读取器会给上限加一检测超额，MaxInt64 不能作为合法的无限值
		return nil, ErrInvalidOptions
	}
	if err := stream.ValidateOrigins(opts.AllowedOrigins); err != nil {
		return nil, ErrInvalidOptions
	}
	if opts.MaxDuration == 0 {
		// 连接必须定期重建，避免长时间闲置会话永远占据服务端容量
		opts.MaxDuration = 5 * time.Minute
	}
	if opts.WriteTimeout == 0 {
		// 保留独立写预算，让慢读者不能阻塞其他连接的停止流程
		opts.WriteTimeout = 5 * time.Second
	}
	if opts.PingInterval == 0 {
		opts.PingInterval = 20 * time.Second
	}
	if opts.PongTimeout == 0 {
		opts.PongTimeout = 10 * time.Second
	}
	if opts.RecheckInterval == 0 {
		// 已连接不等于永久授权，登出与权限回收需要在连接内再次生效
		opts.RecheckInterval = 30 * time.Second
	}
	if opts.MaxMessageBytes == 0 {
		// 这是每条消息的上限，不是整条连接的累计流量配额
		opts.MaxMessageBytes = 64 << 10
	}
	// 拷贝白名单，避免调用者在启动后改动原切片导致来源策略变化或数据竞争
	opts.AllowedOrigins = append([]string(nil), opts.AllowedOrigins...)
	return &Server{manager: manager, opts: opts}, nil
}

// Serve 拥有整个响应；调用者无论成功失败都不能在返回后再写 JSON
// handler 在请求 goroutine 同步执行，必须持续读取以处理 ping/pong 并响应 ctx 取消
func (s *Server) Serve(w http.ResponseWriter, r *http.Request, handler func(context.Context, *Conn) error) error {
	if s == nil || w == nil || r == nil || handler == nil {
		if w != nil {
			stream.Reject(w, 500, "stream_invalid_options", "流连接配置错误")
		}
		return ErrInvalidOptions
	}
	// 已过期的普通请求不能通过切换协议复活；只对尚有效的请求恢复原始取消信号
	if err := r.Context().Err(); err != nil {
		// 返回错误本身不会写状态码；明确拒绝，避免 HTTP 默认为空的 200
		stream.Reject(w, 408, "stream_request_expired", "流连接请求已过期")
		return err
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		stream.Reject(w, 405, "stream_method_not_allowed", "流连接仅支持 GET")
		return ErrInvalidOptions
	}
	if !stream.OriginAllowed(r, s.opts.AllowedOrigins) {
		stream.Reject(w, 403, "origin_forbidden", "浏览器来源未获允许")
		return ErrOriginForbidden
	}
	if !supportsHijack(w) {
		// HTTP/2 不提供 Hijacker；在写出 101 之前明确拒绝，不能依赖类型断言
		stream.Reject(w, 501, "stream_unsupported", "服务器不支持 WebSocket 升级")
		return ErrUnavailable
	}
	session, err := s.manager.Begin(stream.RequestContext(r.Context()), s.opts.MaxDuration)
	// 保留客户端断开信号，只移除普通 HTTP 的短预算；绝不能换成 Background
	// 容量必须在升级前申请，满载时还可以发送标准的 HTTP 503
	if err != nil {
		// 超时与容量不足含义不同，客户端不应把失效请求当作满载重试
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			stream.Reject(w, 408, "stream_request_expired", "流连接请求已过期")
		} else {
			stream.Reject(w, 503, "stream_unavailable", "流连接暂不可用")
		}
		return err
	}
	// End 必须晚于回调与内部任务结束，否则停机 Wait 会提前宣称连接已经释放
	defer session.End()
	ctx, cancel := context.WithCancelCause(session.Context())
	defer cancel(nil)
	if !s.authorized(ctx) {
		if ctx.Err() != nil {
			// 仍在握手前，尚可发送统一 envelope；升级后只能关闭 socket
			stream.Reject(w, 408, "stream_request_expired", "流连接请求已过期")
			return connectionError(ctx, ctx.Err())
		}
		stream.Reject(w, 403, "authorization_failed", "流连接授权已失效")
		return ErrUnauthorized
	}
	// 授权返回与真正升级之间仍可能到期，握手前最后检查一次连接期限
	if err := ctx.Err(); err != nil {
		stream.Reject(w, 408, "stream_request_expired", "流连接请求已过期")
		return connectionError(ctx, err)
	}
	// 先用严格来源规则过滤，再把已验证的实际 origin 作为精确模式交给库
	// 默认端口等价与 IPv6 字面量不能意外变成库 path.Match 的通配符语义
	patterns := make([]string, 0, len(s.opts.AllowedOrigins)+1)
	for _, origin := range s.opts.AllowedOrigins {
		patterns = append(patterns, escapePattern(origin))
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		patterns = append(patterns, escapePattern(origin))
	}
	raw, err := coder.Accept(&deadlineWriter{ResponseWriter: w}, r, &coder.AcceptOptions{OriginPatterns: patterns, CompressionMode: coder.CompressionDisabled})
	if err != nil {
		return ErrHandshake
	} // Accept 已写失败响应，这里绝不能再发送第二个 HTTP 响应
	raw.SetReadLimit(s.opts.MaxMessageBytes)
	// 从这里开始 ResponseWriter 已被接管，只能读写帧，不能再进入 JSON 响应流程
	// 本层不创建应用消息队列；应用读取速度和单次写预算直接传递网络背压
	conn := &Conn{socket: raw, ctx: ctx, cancel: cancel, writeTimeout: s.opts.WriteTimeout, limit: s.opts.MaxMessageBytes}
	var workers sync.WaitGroup
	// WaitGroup 只登记封装自身的任务；业务自行创建的 goroutine 仍由业务 join
	// 先登记再启动，防止极快完成的任务与 Add 发生竞态
	workers.Add(2)
	// 取消 context 本身不会关闭被 hijack 的 TCP socket，必须显式桥接
	// CloseNow 同时唤醒阻塞读取和写入，业务不应靠轮询判断停机状态
	go func() { defer workers.Done(); <-ctx.Done(); _ = raw.CloseNow() }()
	go func() { defer workers.Done(); s.control(ctx, cancel, raw) }()
	defer func() {
		// HTTP Shutdown 不跟踪 hijack 连接；主动关闭让阻塞读写随停机取消一起退出
		// 不等待客户端完成可能耗时十秒的 close handshake，关闭预算由本层掌握
		cancel(ErrClosed)
		_ = raw.CloseNow()
		workers.Wait()
	}()
	if err := callHandler(ctx, conn, handler); err != nil {
		// 回调产生的错误必须重新分类，即使它包装了不可信的连接关闭原因
		return connectionError(ctx, err)
	}
	if ctx.Err() != nil {
		// 正常返回也可能恰逢权限撤销或停机，返回真正取消原因便于上层统计
		return connectionError(ctx, ctx.Err())
	}
	return nil
}

// authorized 也在后台运行，panic 不能逃到 goroutine 顶层终止整个进程
// 对授权失败只传递固定分类，避免数据库错误或凭据进入网络与日志
func (s *Server) authorized(ctx context.Context) (ok bool) {
	defer func() {
		if recover() != nil {
			ok = false
		}
	}()
	if s.opts.Authorize == nil {
		return ctx.Err() == nil
	}
	// 授权查询也有独立预算；回调不能忽略 ctx 后无限等待外部服务
	checkCtx, cancel := context.WithTimeout(ctx, s.opts.WriteTimeout)
	defer cancel()
	return s.opts.Authorize(checkCtx) == nil && checkCtx.Err() == nil
}

func (s *Server) control(ctx context.Context, cancel context.CancelCauseFunc, raw *coder.Conn) {
	// 单个控制循环避免权限检查与心跳无限堆积；每项工作都必须响应自己的预算
	// ticker 在退出时释放，不能把每次连接的计时器留到进程结束
	ping := time.NewTicker(s.opts.PingInterval)
	defer ping.Stop()
	recheck := time.NewTicker(s.opts.RecheckInterval)
	defer recheck.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ping.C:
			// Ping 不负责读 pong，handler 必须持续 Read；等待和控制帧写入共用有限预算
			pingCtx, stop := context.WithTimeout(ctx, s.opts.PongTimeout)
			err := raw.Ping(pingCtx)
			stop()
			if err != nil {
				cancel(ErrClosed)
				return
			}
		case <-recheck.C:
			// 授权失败只触发取消；关闭 watcher 统一负责打断正在阻塞的业务 I/O
			if !s.authorized(ctx) {
				cancel(ErrUnauthorized)
				return
			}
		}
	}
}

func escapePattern(origin string) string {
	// 共享来源校验使用精确比较，而库接受 glob；在交接前消除语义差异
	// IPv6 的方括号尤其需要转义，否则会被 path.Match 解释成字符集合
	return strings.NewReplacer("\\", "\\\\", "[", "\\[", "]", "\\]", "*", "\\*", "?", "\\?").Replace(origin)
}

// 协议开始后交给 Gin Recovery 会尝试写 HTTP 错误，污染已升级的连接
// 在协议边界收敛回调 panic，仍会执行 Serve 的取消、等待与容量释放
func callHandler(ctx context.Context, conn *Conn, handler func(context.Context, *Conn) error) (err error) {
	defer func() {
		if recover() != nil {
			err = ErrHandler
		}
	}()
	if err = handler(ctx, conn); err != nil {
		// 本封装方法已经返回安全分类，保留它们便于业务判断正常离线与授权失败
		for _, known := range []error{ErrInvalidMessage, ErrMessageTooBig, ErrUnauthorized, ErrClosed, ErrUnavailable} {
			if errors.Is(err, known) {
				return known
			}
		}
		return ErrHandler
	}
	return nil
}
