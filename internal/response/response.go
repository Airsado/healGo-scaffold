// Package response owns the transport contract; services return ordinary Go values.
// Keeping HTTP status and the error code together prevents "200 with an error" APIs.
package response

import (
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gin-gonic/gin/binding"
)

const RequestIDKey = "heago.request_id"

// Error Code 是稳定的机器判断依据，Message 面向操作者；客户端不应靠匹配中文提示分支
// 不直接暴露 error 类型，避免序列化时意外携带库错误、SQL 或敏感输入
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Envelope 统一包裹层让业务模块共享前端请求处理逻辑，而不各自定义成功与失败结构
// 毫秒时间戳用于显示和排障，不作为请求签名或身份可信性的依据
type Envelope struct {
	Data      any    `json:"data,omitempty"`
	Error     *Error `json:"error,omitempty"`
	RequestID string `json:"requestId"`
	TimeStamp int64  `json:"timeStamp"`
}

// Success never serializes a database model implicitly; the controller chooses its DTO.
// requestId correlates a response with the sanitized access log, and is not a credential.
func Success(c *gin.Context, data any) { Write(c, http.StatusOK, data) }
func Write(c *gin.Context, status int, data any) {
	// 保留显式 status 参数，创建资源等场景可以正确返回 201，而不是一律包装成 200
	c.JSON(status, Envelope{Data: data, RequestID: c.GetString(RequestIDKey), TimeStamp: time.Now().UnixMilli()})
}
func Fail(c *gin.Context, status int, code, message string) {
	// Abort 阻止后续处理器继续执行，避免已经拒绝的请求仍发生业务写入
	c.AbortWithStatusJSON(status, Envelope{Error: &Error{Code: code, Message: message}, RequestID: c.GetString(RequestIDKey), TimeStamp: time.Now().UnixMilli()})
}

// BindJSON rejects unknown fields and trailing JSON rather than silently accepting typos.
// Validation errors describe the rule without echoing request values such as passwords.
// Body size is enforced by the HTTP middleware before the decoder reads the stream.
func BindJSON(c *gin.Context, value any) bool {
	// 不按正文内容猜测媒体类型，让客户端错误与 JSON 解码错误有明确的边界
	media, _, err := mime.ParseMediaType(c.GetHeader("Content-Type"))
	if err != nil || media != "application/json" {
		Fail(c, 415, "unsupported_media_type", "Content-Type 必须为 application/json")
		return false
	}
	decoder := json.NewDecoder(c.Request.Body)
	// 未知字段通常意味着接口拼写错误；悄悄忽略可能让操作者误以为配置已生效
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(value); err == nil {
		// 只允许一个 JSON 值，拒绝“合法对象后再附带另一个对象”的模糊请求
		var trailing any
		if e := decoder.Decode(&trailing); !errors.Is(e, io.EOF) {
			// Preserve size/read errors from the second decode, including a body
			// whose first JSON value fits but whose trailing bytes exceed the limit.
			err = e
			if e == nil {
				err = errors.New("trailing JSON")
			}
		}
	}
	if err != nil {
		var large *http.MaxBytesError
		// 使用 errors.As 保留包装后的体积错误分类，413 与普通格式错误 400 含义不同
		if errors.As(err, &large) {
			Fail(c, 413, "body_too_large", "请求体超出大小限制")
		} else {
			Fail(c, 400, "invalid_json", "JSON 格式、字段或请求长度不正确")
		}
		return false
	}
	if binding.Validator.ValidateStruct(value) != nil {
		// 标签校验发生在结构正确之后；业务合法性仍由服务层规则与数据库约束保证
		Fail(c, 400, "invalid_input", "请求参数未通过校验")
		return false
	}
	return true
}
