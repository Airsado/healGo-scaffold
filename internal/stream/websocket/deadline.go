package websocket

import (
	"bufio"
	"net"
	"net/http"
	"time"
)

// coder v1.8.15 Accept 保留 net/http 的旧连接期限，必须在接管连接的瞬间清除
// 清除发生在实际 net.Conn 上，不假设每个 ResponseWriter 包装器都实现 deadline API
type deadlineWriter struct {
	// 这个包装器只在同步握手中使用；升级后的并发 I/O 直接使用协议连接
	// upgraded 是防止错误路径重复写 HTTP 的状态，不是业务连接状态
	http.ResponseWriter
	upgraded bool
}

func (w *deadlineWriter) WriteHeader(status int) {
	// 升级后只使用 WebSocket 帧，失败路径也不能再次往 HTTP 响应写 JSON 或文本
	if w.upgraded {
		return
	}
	w.upgraded = status == http.StatusSwitchingProtocols
	w.ResponseWriter.WriteHeader(status)
}

func (w *deadlineWriter) Write(payload []byte) (int, error) {
	if w.upgraded {
		return len(payload), nil
	}
	return w.ResponseWriter.Write(payload)
}

func (w *deadlineWriter) WriteHeaderNow() {
	// Gin 的 WriteHeader 只记状态；升级前必须让它真正提交 101 响应头
	if writer, ok := w.ResponseWriter.(interface{ WriteHeaderNow() }); ok {
		writer.WriteHeaderNow()
	}
}

func (w *deadlineWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	// ResponseController 兼容普通包装器的 Unwrap，不强迫它们重写 Hijacker
	// 失败时仍由 Accept 收敛握手结果，调用者不能再发送第二份 HTTP 响应
	conn, buffer, err := http.NewResponseController(w.ResponseWriter).Hijack()
	if err != nil {
		return nil, nil, err
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		// 无法清除继承期限时不能提供看似正常、稍后突然超时的连接
		_ = conn.Close()
		return nil, nil, ErrUnavailable
	}
	return conn, buffer, nil
}

func supportsHijack(w http.ResponseWriter) bool {
	// 沿 Unwrap 检查最内层能力，避免 Gin 在 HTTP/2 下的 Hijack 类型断言 panic
	for {
		// Gin 在接口层声明 Hijack，因此还要检查它真正包装的底层 writer
		if wrapper, ok := w.(interface{ Unwrap() http.ResponseWriter }); ok {
			w = wrapper.Unwrap()
			continue
		}
		_, ok := w.(http.Hijacker)
		return ok
	}
}
