package websocket

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"time"
	"unicode/utf8"

	coder "github.com/coder/websocket"
)

// MessageType 只复用协议类型；连接对象和底层错误均留在本包的边界内
type MessageType = coder.MessageType

const (
	MessageText   = coder.MessageText
	MessageBinary = coder.MessageBinary
)

// Conn 不暴露底层 socket，防止调用者绕过消息大小、写入期限和生命周期约束
// 仅一个 goroutine 可拥有读取循环；写操作可并发，库会在各自 context 内串行化
type Conn struct {
	socket       *coder.Conn
	ctx          context.Context
	cancel       context.CancelCauseFunc
	writeTimeout time.Duration
	limit        int64
}

// Read 一次读取完整消息；底层读取限制在分配完整消息之前生效
// 只有一个读取者可以拥有此循环，持续读取也负责响应远端控制帧
func (c *Conn) Read(ctx context.Context) (MessageType, []byte, error) {
	opCtx, done, err := c.operationContext(ctx, 0)
	if err != nil {
		return 0, nil, err
	}
	defer done()
	typ, payload, err := c.socket.Read(opCtx)
	if err != nil {
		return 0, nil, c.ioError(opCtx, err)
	}
	// 文本帧必须是 UTF-8；二进制帧保留原始字节，不能偷偷替换无效字符
	if typ == MessageText && !utf8.Valid(payload) {
		c.cancel(ErrInvalidMessage)
		return 0, nil, ErrInvalidMessage
	}
	return typ, payload, nil
}

// Write 不创建发送队列；背压由调用者感知，超时后连接会被底层库关闭
// 本地格式错误尚未发送任何帧，调用者可以修正消息后继续使用连接
func (c *Conn) Write(ctx context.Context, typ MessageType, payload []byte) error {
	if c == nil || c.socket == nil {
		return ErrClosed
	}
	if typ != MessageText && typ != MessageBinary {
		return ErrInvalidMessage
	}
	if int64(len(payload)) > c.limit {
		return ErrMessageTooBig
	}
	if typ == MessageText && !utf8.Valid(payload) {
		return ErrInvalidMessage
	}
	// 从等待写锁开始就计算超时，不能让排在慢客户端后面的写入无界等待
	opCtx, done, err := c.operationContext(ctx, c.writeTimeout)
	if err != nil {
		return err
	}
	defer done()
	return c.ioError(opCtx, c.socket.Write(opCtx, typ, payload))
}

// ReadJSON 只接受文本消息，避免二进制与 JSON 协议意外混用
// JSON 错误不包含原始 payload；业务仍负责字段权限与领域规则验证
func (c *Conn) ReadJSON(ctx context.Context, value any) error {
	typ, payload, err := c.Read(ctx)
	if err != nil {
		return err
	}
	if typ != MessageText || json.Unmarshal(payload, value) != nil {
		c.cancel(ErrInvalidMessage)
		return ErrInvalidMessage
	}
	return nil
}

// WriteJSON 先编码再走统一大小与写入预算，因此不会绕过 Write 的边界
// 调用者应构建有界对象；此限额不负责限制自定义 MarshalJSON 内部的内存
func (c *Conn) WriteJSON(ctx context.Context, value any) error {
	// JSON 编码错误可能包含应用对象细节，只返回固定错误，不能向日志传原始错误
	payload, err := json.Marshal(value)
	if err != nil {
		return ErrInvalidMessage
	}
	return c.Write(ctx, MessageText, payload)
}

func (c *Conn) operationContext(parent context.Context, timeout time.Duration) (context.Context, func(), error) {
	// 每次 I/O 都有自己的取消桥接，返回时停止 AfterFunc，避免闭包长期持有请求
	// 调用者更短的 deadline 仍然生效，连接级期限只是另一个必须同时满足的边界
	if c == nil || c.socket == nil {
		return nil, nil, ErrClosed
	}
	if parent == nil {
		return nil, nil, ErrInvalidOptions
	}
	if c.ctx.Err() != nil {
		return nil, nil, connectionError(c.ctx, c.ctx.Err())
	}
	ctx, cancel := context.WithCancelCause(parent)
	// 即使业务误用 Background，连接级停机仍能取消当前 I/O；值与调用者期限仍保留
	// 保留授权失效等原因；只传播普通 canceled 会让调用者丢失重新登录的依据
	stop := context.AfterFunc(c.ctx, func() { cancel(context.Cause(c.ctx)) })
	if timeout > 0 {
		// WithTimeout 选取更早的父期限，不能因为框架默认值而延长调用者预算
		bounded, finish := context.WithTimeout(ctx, timeout)
		return bounded, func() { finish(); stop(); cancel(nil) }, nil
	}
	return ctx, func() { stop(); cancel(nil) }, nil
}

// socket 的关闭可能先于 AfterFunc 的调度，直接检查连接 context 避免丢失原因
// 这里只返回固定分类与标准 context 错误，不传播驱动错误中的远端内容
func (c *Conn) ioError(ctx context.Context, err error) error {
	if err != nil && c.ctx.Err() != nil {
		return connectionError(c.ctx, err)
	}
	return connectionError(ctx, err)
}

func connectionError(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	// 原连接的取消原因优先于关闭 socket 产生的次生错误；授权撤销应可识别
	if ctx.Err() != nil {
		for _, known := range []error{ErrUnauthorized, ErrInvalidMessage} {
			if errors.Is(context.Cause(ctx), known) {
				return known
			}
		}
		return errors.Join(ErrClosed, ctx.Err())
	}
	// 不返回或包装 coder.CloseError，因为其 reason 完全由远端控制，可能包含秘密
	for _, known := range []error{ErrInvalidMessage, ErrMessageTooBig, ErrUnauthorized, ErrClosed, ErrUnavailable, ErrHandler} {
		if errors.Is(err, known) {
			return known
		}
	}
	if errors.Is(err, coder.ErrMessageTooBig) {
		// 大小限制是本地可信分类，但库附带的诊断文本仍无需透出
		return ErrMessageTooBig
	}
	if coder.CloseStatus(err) >= 0 || errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
		// 不向上层传递关闭码文本，业务可以用 ErrClosed 统一处理断开
		return ErrClosed
	}
	return ErrUnavailable
}
