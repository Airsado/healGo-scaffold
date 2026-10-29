## 职责

统一 HTTP 响应结构和 JSON 输入边界，让控制器选择明确的状态码、错误码与 DTO。服务层返回普通 Go 值和稳定错误，不反向依赖 Gin 或在此拼装业务结果。

## 内容

[response.go](response.go) 定义 `Envelope`，提供成功、失败及指定状态响应；`BindJSON` 拒绝错误媒体类型、未知字段、尾随 JSON 和未通过标签校验的输入。

## 约定

请求标识由中间件提前设置；正文大小限制也由中间件安装。失败响应终止后续处理链，不返回原始库错误。格式问题用 400，超限用 413，媒体类型错误用 415；业务合法性仍交给服务层判断。

在根目录执行 `go test ./pkg/router -run TestHTTPBoundariesCORSAndSanitizedRecovery`，覆盖输入、错误结构及脱敏行为，不需要 PostgreSQL。
