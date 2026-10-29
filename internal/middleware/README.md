## 职责

在控制器之前处理通用 HTTP 行为，建立请求标识、资源限制与访问边界。这里不查询具体业务数据，也不把“已经登录”等同于“拥有全部权限”。

## 内容

[http.go](http.go) 提供安全访问日志、panic 恢复、正文限制、context 超时、CORS 和登录容量限制；[auth.go](auth.go) 校验身份，并提供权限、角色及身份上下文访问器。

## 约定

由路由层按顺序注册；认证只信任服务校验结果。context 超时需要下游协作，不强制终止 goroutine。CORS 不替代认证；登录预算是单进程容量保护。处理器不能长期保存 Gin context，日志只使用安全字段。

在根目录执行 `go test ./pkg/router -run TestHTTPBoundariesCORSAndSanitizedRecovery`，验证无需数据库的 HTTP 边界。
