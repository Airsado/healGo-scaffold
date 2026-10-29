// Package stream 管理显式开启的长连接；普通 HTTP 继续使用原来的请求超时
// 这里不维护业务订阅、消息历史或广播队列，只负责容量、取消与生命周期
package stream

import (
	"context"
	"errors"
	"sync"
	"time"
)

var (
	ErrClosed         = errors.New("stream manager is closed")
	ErrCapacity       = errors.New("stream connection limit reached")
	ErrInvalidOptions = errors.New("invalid stream options")
)

// Manager 是每个应用实例独立的连接登记表，不是跨实例的在线状态存储
// 容量由实际回调结束后释放，不能在只发出取消信号时就宣称连接已经结束
type Manager struct {
	// 只用一把锁保护状态和登记表，计数直接来自 map，避免两份状态逐渐不一致
	mu       sync.Mutex
	limit    int
	closed   bool
	sessions map[*Session]struct{}
	idle     chan struct{}
}

func NewManager(maxConnections int) (*Manager, error) {
	if maxConnections < 1 {
		return nil, ErrInvalidOptions
	}
	// 空登记表一开始就处于排空状态；用已关闭的 channel 表达，Wait 无需轮询
	// 第一个连接进入时再换成新的 channel，后续连接共享同一轮排空信号
	idle := make(chan struct{})
	close(idle)
	return &Manager{limit: maxConnections, sessions: make(map[*Session]struct{}), idle: idle}, nil
}

// Session 只持有连接级 context，不持有 Gin Context 或业务对象
// End 由协议封装在回调和内部任务全部退出后调用，而非交给业务记住清理
type Session struct {
	manager *Manager
	ctx     context.Context
	cancel  context.CancelFunc
	end     sync.Once
}

// Begin 必须在写出 SSE 响应或升级 WebSocket 之前调用，容量不足时仍可返回 HTTP 错误
// 到期只取消工作；调用者 defer End 才能保证 Wait 等到所有回调真正返回
func (m *Manager) Begin(parent context.Context, maxDuration time.Duration) (*Session, error) {
	if m == nil || parent == nil || maxDuration <= 0 {
		return nil, ErrInvalidOptions
	}
	if err := parent.Err(); err != nil {
		// 不为已经断开的请求占位，也不启动一条马上就会到期的新计时器
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		// Close 是不可逆的应用停机操作；需要新生命周期时创建新的 Manager
		return nil, ErrClosed
	}
	if len(m.sessions) >= m.limit {
		return nil, ErrCapacity
	}
	if len(m.sessions) == 0 {
		m.idle = make(chan struct{})
	}
	// 同一把锁覆盖容量判断和登记，避免并发请求同时看到最后一个空位
	ctx, cancel := context.WithTimeout(parent, maxDuration)
	s := &Session{manager: m, ctx: ctx, cancel: cancel}
	m.sessions[s] = struct{}{}
	return s, nil
}

// Context 同时传播客户端断开、最长时长和应用停机；协议内部任务使用同一个源
// 业务可派生更短的 I/O 期限，但不能把该 context 替换成 Background
func (s *Session) Context() context.Context { return s.ctx }

// End 可以由多条退出路径重复调用，sync.Once 保证容量只释放一次
// 先取消剩余 I/O，再更新登记表；最后一个连接通知所有等待停机的调用者
func (s *Session) End() {
	if s == nil {
		return
	}
	s.end.Do(func() {
		s.cancel()
		m := s.manager
		// End 与 Begin/Close 使用同一把锁，最后一个连接只会关闭一次 idle
		m.mu.Lock()
		defer m.mu.Unlock()
		delete(m.sessions, s)
		if len(m.sessions) == 0 {
			close(m.idle)
		}
	})
}

// Close 先阻止新连接，再取消现有工作；不在持锁状态执行用户提供的清理回调
// 它不会等待不合作的业务代码，停机预算由 Wait 的 context 明确约束
func (m *Manager) Close() {
	if m == nil {
		return
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.closed = true
	// 复制取消函数后解锁context 的取消传播不应长期占据登记表锁
	// 已复制的 cancel 可与 End 并发执行，Go 的 CancelFunc 本身允许重复调用
	cancels := make([]context.CancelFunc, 0, len(m.sessions))
	for s := range m.sessions {
		cancels = append(cancels, s.cancel)
	}
	m.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
}

// Wait 与 Close 分离，让 HTTP Shutdown 和已 hijack 的 WebSocket 共用同一个停机预算
// HTTP Server 自己并不追踪 hijack 后的连接，不能仅依赖 server.Shutdown
func (m *Manager) Wait(ctx context.Context) error {
	if m == nil {
		return nil
	}
	if ctx == nil {
		return ErrInvalidOptions
	}
	m.mu.Lock()
	// 保存 channel 后释放锁，让活动连接有机会调用 End 触发通知
	idle := m.idle
	m.mu.Unlock()
	// Wait 观察当前一轮连接；停机时先调用 Close，才能禁止出现下一轮新连接
	// 普通运行期间调用 Wait 仅是快照等待，不能把它当成拒绝新连接的操作
	select {
	case <-idle:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Active 返回当前登记数，包含已取消但回调尚未结束的连接
// 这使容量与真正占用的业务工作一致，避免取消后立即接受无限新工作
func (m *Manager) Active() int {
	if m == nil {
		return 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.sessions)
}
