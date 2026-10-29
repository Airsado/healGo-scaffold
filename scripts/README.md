# 初始化脚本

## 职责

首次生成私有部署文件，使数据库连接、管理员初始凭据和加密主密钥具有可复用的启动入口。

## 内容

- [init-config.ps1](init-config.ps1)：PowerShell 7 脚本，通过系统加密随机源生成凭据。
- [init-config.sh](init-config.sh)：POSIX shell 脚本，使用 OpenSSL 生成凭据并设置文件权限。
- 两个脚本读取[配置示例](../configs/config.example.yaml)，生成本机及 Docker YAML、`.local/postgres_password` 和只含宿主组 ID 的 `.local/compose.env`，不输出密钥。
- [replication/](replication/README.md)：独立的 PostgreSQL/MySQL 真实主从复制验收环境，用于验证读写路由、延迟及节点故障；它不修改部署用 Compose。

## 约定

镜像构建使用 Dockerfile 中的 `GOPROXY` 参数；可按网络环境运行 `docker compose --env-file .local/compose.env build --build-arg GOPROXY=https://goproxy.cn,direct`，再执行 `up -d`。它只影响本次构建，不修改宿主机 Go 设置。

- 在模板根目录执行 `pwsh -File scripts/init-config.ps1` 或 `sh scripts/init-config.sh`；不要使用 shell 跟踪模式输出密钥变量。
- 已完整生成时重复运行不覆盖文件；只有部分文件存在时直接停止，由维护者检查原因。
- 脚本不修改数据库密码、账号或运行时配置。修改私有文件不会自动改变已经初始化的 PostgreSQL 角色。
- 私有生成文件不提交；加密主密钥在数据库之外单独备份。变更输出或权限策略时应同步验证两个脚本。
