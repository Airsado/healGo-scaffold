// Package auth owns persisted identities and adapts Sa-Token to application RBAC.
// Business modules depend on Principal and permission names, never password hashes.
//
// 认证分两层：主数据库保留账号与权限，内存只保留有期限的随机会话
// 因而重启要求重新登录，但账号、角色、密码与禁用状态不会随进程丢失
// 控制器先认证得到 Principal，再为每个受保护操作声明所需权限
// 业务模块复用这些入口，不读取 sys_users.password_hash，也不创建另一套 JWT
// 用户和角色管理属于框架基础能力，不携带订单、机器人等业务模型
//
// 文件里的初始账号只在空库启动时使用一次，之后由数据库成为事实来源
// 数据表初始化则交给组合根注册 Models，避免 import 隐式执行迁移
// 数据库实例本身的创建、账号授权和备份恢复仍属于部署职责
// 所有写操作接受 context，让请求超时和停机取消能传递到数据库连接
//
// 权限列表按请求刷新，而 token 的总有效期固定，不会因为访问而自动延长
// 修改密码或禁用账号增加 AuthVersion，数据库提交就是旧会话失效边界
// 一次请求已经完成的操作不能被回溯撤销，后续授权检查会读取最新版本
// 水平扩容需要共享会话存储；当前模板明确面向单应用实例
package auth

import "time"

// User is a database entity, not a response DTO. Explicit json exclusions make an
// accidental JSON marshal safe even when a controller forgets to use Principal.
type User struct {
	// AuthVersion 是撤销凭据的数据库版本号，不是普通资料更新时间
	// token 对应旧版本时立即拒绝，因此修改密码不需要扫描全部会话
	// PasswordHash 只用于校验，任何列表或 /me 接口都使用 Principal
	ID           uint64    `gorm:"primaryKey" json:"id,string"`
	Username     string    `gorm:"size:64;not null;uniqueIndex" json:"username"`
	PasswordHash string    `gorm:"size:100;not null" json:"-"`
	Enabled      bool      `gorm:"not null" json:"enabled"`
	AuthVersion  uint64    `gorm:"not null;default:1" json:"-"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

func (User) TableName() string { return "sys_users" }

// Roles contain no special administrator flag. The initial admin role receives
// the ordinary '*' permission, so all authorization follows the same code path.
type Role struct {
	// 角色名称用于权限分组，具体权限仍由关联表维护
	// 不把权限列表塞入逗号字符串，数据库才能约束重复和关联关系
	ID        uint64    `gorm:"primaryKey" json:"id,string"`
	Name      string    `gorm:"size:64;not null;uniqueIndex" json:"name"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (Role) TableName() string { return "sys_roles" }

// Join-table primary keys forbid duplicate assignments at the database boundary.
// Foreign keys remove orphan assignments if an operator deliberately deletes a row.
type UserRole struct {
	UserID uint64 `gorm:"primaryKey" json:"-"`
	RoleID uint64 `gorm:"primaryKey" json:"-"`
	User   User   `gorm:"foreignKey:UserID;constraint:OnDelete:CASCADE" json:"-"`
	Role   Role   `gorm:"foreignKey:RoleID;constraint:OnDelete:CASCADE" json:"-"`
}

func (UserRole) TableName() string { return "sys_user_roles" }

type RolePermission struct {
	// 联合主键允许多个角色共享同一个权限名，同时禁止角色内重复权限
	// '*' 是 Sa-Token 支持的通配符，与其他权限一样保存和读取
	RoleID     uint64 `gorm:"primaryKey" json:"-"`
	Permission string `gorm:"primaryKey;size:128" json:"permission"`
	Role       Role   `gorm:"foreignKey:RoleID;constraint:OnDelete:CASCADE" json:"-"`
}

func (RolePermission) TableName() string { return "sys_role_permissions" }

// Models keeps migration registration explicit: importing a package has no DDL
// side effects, and the composition root chooses when GORM initializes tables.
func Models() []any { return []any{&User{}, &Role{}, &UserRole{}, &RolePermission{}} }

// Principal is the sole public identity representation. String IDs avoid losing
// integer precision when the API is consumed by a JavaScript administrator UI.
type Principal struct {
	// Roles 和 Permissions 返回空数组，调用方无需区分 null 与无权限
	// authVersion 不导出，客户端无法靠修改请求 JSON 冒充认证状态
	ID          uint64   `json:"id,string"`
	Username    string   `json:"username"`
	Enabled     bool     `json:"enabled"`
	Roles       []string `json:"roles"`
	Permissions []string `json:"permissions"`
	authVersion uint64
}

type RoleInfo struct {
	// 管理 API 展示的是角色名称和权限，不暴露框架内部的关系表 ID
	// 后续业务按稳定名称声明访问要求，数据库 ID 仍可独立迁移
	Name        string   `json:"name"`
	Permissions []string `json:"permissions"`
}
