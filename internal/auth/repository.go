package auth

import (
	"context"
	"errors"
	"regexp"
	"sort"
	"strings"

	"healgo-scaffold/internal/db"

	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// A database-wide lock serializes bootstrap and administrator mutations across
// application processes. The database adapter owns dialect-specific lock lifetime.
const identityLock int64 = 6846420395409

func identityTransaction(ctx context.Context, database *gorm.DB, fn func(*gorm.DB) error) error {
	// PostgreSQL 事务锁和 MySQL 连接锁统一封装在数据库层
	// 所有会影响管理员数量的写入共用这把锁，避免并发分别看见旧数量
	// 锁必须覆盖事务提交，不能在提交前释放后让另一进程读到旧快照
	err := db.LockedTransaction(ctx, database, identityLock, fn)
	return safeDBError(err)
}

// Bootstrap is a seed, never a synchronization job. Once any identity exists,
// changed file credentials do not silently reset passwords or grant new access.
func (s *Service) Bootstrap(ctx context.Context, username, password string) error {
	return identityTransaction(ctx, s.db, func(tx *gorm.DB) error {
		var count int64
		if err := tx.Model(&User{}).Count(&count).Error; err != nil {
			return err
		}
		if count != 0 {
			// 已初始化时连 seed 校验都不参与账号更新；文件不是密码重置工具
			// 想修改凭据必须经过认证 API，而不是悄悄替换部署配置
			return nil
		}
		if !validName(username) || !validPassword(password) {
			return ErrInvalidInput
		}
		hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
		if err != nil {
			return ErrInvalidInput
		}
		role := Role{Name: "admin"}
		// 用户、角色、权限、关系必须在同一事务写入
		// 任一步失败全部回滚，避免残留“有用户但没管理员”的半初始化库
		if err := tx.Create(&role).Error; err != nil {
			return err
		}
		permission := RolePermission{RoleID: role.ID, Permission: "*"}
		if err := tx.Omit(clause.Associations).Create(&permission).Error; err != nil {
			return err
		}
		user := User{Username: username, PasswordHash: string(hash), Enabled: true, AuthVersion: 1}
		if err := tx.Create(&user).Error; err != nil {
			return err
		}
		return tx.Omit(clause.Associations).Create(&UserRole{UserID: user.ID, RoleID: role.ID}).Error
	})
}

// One joined SELECT gives each principal a consistent snapshot of identity,
// roles and permissions. No response path loads the password_hash column.
type principalRow struct {
	ID          uint64
	Username    string
	Enabled     bool
	AuthVersion uint64
	RoleName    *string
	Permission  *string
}

func (s *Service) principal(ctx context.Context, id uint64) (Principal, error) {
	// 读取不存在账号与读取失败是不同情况，认证层会将前者映射为 401
	// 这样数据库故障不会掩盖成账号不存在，也不会意外降级为允许访问
	items, err := s.principals(ctx, []uint64{id})
	if err != nil {
		return Principal{}, err
	}
	if len(items) == 0 {
		return Principal{}, ErrNotFound
	}
	return items[0], nil
}

func (s *Service) principals(ctx context.Context, ids []uint64) ([]Principal, error) {
	// 角色与权限是多对多关系，连接查询会为一个用户返回多行
	// 下面集中去重并构造 DTO，控制器不接触关系表或密码字段
	result := make([]Principal, 0, len(ids))
	if len(ids) == 0 {
		return result, nil
	}
	var rows []principalRow
	err := s.db.WithContext(ctx).Table("sys_users AS u").
		Select("u.id, u.username, u.enabled, u.auth_version, r.name AS role_name, rp.permission").
		Joins("LEFT JOIN sys_user_roles AS ur ON ur.user_id = u.id").
		Joins("LEFT JOIN sys_roles AS r ON r.id = ur.role_id").
		Joins("LEFT JOIN sys_role_permissions AS rp ON rp.role_id = r.id").
		Where("u.id IN ?", ids).Order("u.id, r.name, rp.permission").Scan(&rows).Error
	if err != nil {
		return nil, ErrUnavailable
	}
	byID := make(map[uint64]*Principal, len(ids))
	for _, row := range rows {
		p := byID[row.ID]
		if p == nil {
			p = &Principal{ID: row.ID, Username: row.Username, Enabled: row.Enabled,
				authVersion: row.AuthVersion, Roles: []string{}, Permissions: []string{}}
			byID[row.ID] = p
		}
		if row.RoleName != nil && !contains(p.Roles, *row.RoleName) {
			p.Roles = append(p.Roles, *row.RoleName)
		}
		if row.Permission != nil && !contains(p.Permissions, *row.Permission) {
			p.Permissions = append(p.Permissions, *row.Permission)
		}
	}
	for _, id := range ids {
		// 按输入 ID 顺序输出，让分页顺序独立于数据库连接计划
		// 权限排序使响应和测试稳定，客户端比较配置时不会产生假变化
		if p := byID[id]; p != nil {
			sort.Strings(p.Permissions)
			result = append(result, *p)
		}
	}
	return result, nil
}

func (s *Service) ListUsers(ctx context.Context, limit, offset int) ([]Principal, int64, error) {
	// 框架层就限制分页上限，不依赖每个未来业务控制器记得防护
	// count 与列表是两次读，期间新增用户只影响显示总数，不影响授权
	if limit < 1 || limit > 100 || offset < 0 || offset > 1_000_000 {
		return nil, 0, ErrInvalidInput
	}
	var total int64
	var ids []uint64
	query := s.db.WithContext(ctx).Model(&User{})
	if err := query.Count(&total).Error; err != nil {
		return nil, 0, ErrUnavailable
	}
	// The joined RBAC query is batched, avoiding a query for every displayed user.
	if err := query.Order("id").Limit(limit).Offset(offset).Pluck("id", &ids).Error; err != nil {
		return nil, 0, ErrUnavailable
	}
	items, err := s.principals(ctx, ids)
	return items, total, err
}

func (s *Service) CreateUser(ctx context.Context, username, password string, roles []string) (Principal, error) {
	// 先验证输入再做昂贵 bcrypt；该接口应只挂在用户管理写权限之后
	// 存库的是自带随机盐的 bcrypt hash，重复密码也不会产生相同 hash
	// 数据库唯一索引是最终约束，事务锁同时保证框架写入的可预测错误
	if !validName(username) || !validPassword(password) || !validNames(roles) {
		return Principal{}, ErrInvalidInput
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return Principal{}, ErrInvalidInput
	}
	user := User{Username: username, PasswordHash: string(hash), Enabled: true, AuthVersion: 1}
	err = identityTransaction(ctx, s.db, func(tx *gorm.DB) error {
		var existing int64
		if err := tx.Model(&User{}).Where("username = ?", username).Count(&existing).Error; err != nil {
			return err
		}
		if existing != 0 {
			return ErrConflict
		}
		if err := tx.Create(&user).Error; err != nil {
			return err
		}
		return assignRoles(tx, user.ID, roles)
	})
	if err != nil {
		return Principal{}, err
	}
	return s.principal(ctx, user.ID)
}

func (s *Service) SetUserRoles(ctx context.Context, id uint64, roles []string) error {
	// 使用整组替换语义，客户端保存后的状态与数据库一一对应
	// 删除旧关系与新增关系必须原子提交，否则认证可能读到临时空角色
	// 最后一个管理员检查在同一事务完成，拒绝时旧关系自动恢复
	if id == 0 || !validNames(roles) {
		return ErrInvalidInput
	}
	return identityTransaction(ctx, s.db, func(tx *gorm.DB) error {
		var user User
		if err := tx.First(&user, id).Error; err != nil {
			return err
		}
		if err := tx.Where("user_id = ?", id).Delete(&UserRole{}).Error; err != nil {
			return err
		}
		if err := assignRoles(tx, id, roles); err != nil {
			return err
		}
		return requireEnabledAdmin(tx)
	})
}

func assignRoles(tx *gorm.DB, userID uint64, names []string) error {
	// 角色必须先被显式创建，拼错角色名不会顺手生成一个新权限组
	// 空列表允许普通账号暂时没有角色，但管理员恢复约束仍由调用方执行
	if len(names) == 0 {
		return nil
	}
	var roles []Role
	if err := tx.Where("name IN ?", names).Find(&roles).Error; err != nil {
		return err
	}
	if len(roles) != len(names) {
		return ErrInvalidInput
	}
	assignments := make([]UserRole, 0, len(roles))
	for _, role := range roles {
		assignments = append(assignments, UserRole{UserID: userID, RoleID: role.ID})
	}
	return tx.Omit(clause.Associations).Create(&assignments).Error
}

func (s *Service) SetEnabled(ctx context.Context, id uint64, enabled bool) error {
	// 禁用保留用户与操作身份，未来审计日志仍能关联同一用户 ID
	// 状态没变时不增加版本，重试同一个请求不会反复撤销新会话
	if id == 0 {
		return ErrInvalidInput
	}
	return identityTransaction(ctx, s.db, func(tx *gorm.DB) error {
		var user User
		if err := tx.First(&user, id).Error; err != nil {
			return err
		}
		if user.Enabled == enabled {
			return nil
		}
		// Changing the version means reenabling a user never revives tokens issued
		// before their account was disabled, even if those tokens have not expired.
		if err := tx.Model(&user).Updates(map[string]any{"enabled": enabled,
			"auth_version": gorm.Expr("auth_version + 1")}).Error; err != nil {
			return err
		}
		return requireEnabledAdmin(tx)
	})
}

func requireEnabledAdmin(tx *gorm.DB) error {
	// “最后管理员”按当前事务里的角色分配和启用状态计算
	// 必须放在写入之后检查，出错回滚比在写入前猜测未来状态更可靠
	var count int64
	err := tx.Table("sys_users AS u").Joins("JOIN sys_user_roles AS ur ON ur.user_id = u.id").
		Joins("JOIN sys_roles AS r ON r.id = ur.role_id").
		Where("u.enabled = ? AND r.name = ?", true, "admin").Count(&count).Error
	if err != nil {
		return err
	}
	// This is an account-recovery invariant, not an administrator authorization
	// bypass. Authorization still evaluates the persisted '*' permission.
	if count == 0 {
		return ErrForbidden
	}
	return nil
}

func (s *Service) ChangePassword(ctx context.Context, id uint64, oldPassword, newPassword string) error {
	// 只提供自助改密，不隐藏一个绕过旧密码的管理重置入口
	// 持久化版本与 hash 同时提交，旧密码和旧 token 在同一边界失效
	// bcrypt 的长度约束按字节计算，中文密码同样受 72 字节上限约束
	if id == 0 || !validPassword(oldPassword) || !validPassword(newPassword) {
		return ErrInvalidInput
	}
	return safeDBError(s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var user User
		// Row locking prevents two simultaneous old-password verifications from
		// both succeeding and silently replacing one another's password update.
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&user, id).Error; err != nil {
			return err
		}
		if !user.Enabled || bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(oldPassword)) != nil {
			return ErrInvalidCredentials
		}
		hash, err := bcrypt.GenerateFromPassword([]byte(newPassword), bcrypt.DefaultCost)
		if err != nil {
			return ErrInvalidInput
		}
		return tx.Model(&user).Updates(map[string]any{"password_hash": string(hash),
			"auth_version": gorm.Expr("auth_version + 1")}).Error
	}))
}

func (s *Service) ListRoles(ctx context.Context) ([]RoleInfo, error) {
	// 这是管理展示查询，不是授权数据源；授权走单条连接快照查询
	// 返回独立 DTO，未来修改关系表结构不会迫使所有业务改 API
	var roles []Role
	var permissions []RolePermission
	if err := s.db.WithContext(ctx).Order("name").Find(&roles).Error; err != nil {
		return nil, ErrUnavailable
	}
	if err := s.db.WithContext(ctx).Order("permission").Find(&permissions).Error; err != nil {
		return nil, ErrUnavailable
	}
	byRole := make(map[uint64][]string, len(roles))
	for _, p := range permissions {
		byRole[p.RoleID] = append(byRole[p.RoleID], p.Permission)
	}
	result := make([]RoleInfo, 0, len(roles))
	for _, role := range roles {
		perms := byRole[role.ID]
		if perms == nil {
			perms = []string{}
		}
		result = append(result, RoleInfo{Name: role.Name, Permissions: perms})
	}
	return result, nil
}

func (s *Service) PutRole(ctx context.Context, name string, permissions []string) error {
	// PutRole 按名称创建或完整替换权限，重复请求具有相同最终结果
	// 权限条目逐项验证并去重，避免列表膨胀和含糊的通配符写法
	// 它不解释业务权限含义，只负责存储并复用 Sa-Token 的匹配规则
	if !validName(name) || len(permissions) > 100 {
		return ErrInvalidInput
	}
	seen := make(map[string]bool, len(permissions))
	for _, permission := range permissions {
		if !validPermission(permission) || seen[permission] {
			return ErrInvalidInput
		}
		seen[permission] = true
	}
	// Keeping this seed role recoverable avoids an API change accidentally
	// removing the only way to administer users. Other roles are unrestricted.
	if name == "admin" && (len(permissions) != 1 || permissions[0] != "*") {
		return ErrForbidden
	}
	return identityTransaction(ctx, s.db, func(tx *gorm.DB) error {
		var role Role
		err := tx.Where("name = ?", name).First(&role).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			role = Role{Name: name}
			if err = tx.Create(&role).Error; err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
		if err := tx.Where("role_id = ?", role.ID).Delete(&RolePermission{}).Error; err != nil {
			return err
		}
		for _, permission := range permissions {
			p := RolePermission{RoleID: role.ID, Permission: permission}
			if err := tx.Omit(clause.Associations).Create(&p).Error; err != nil {
				return err
			}
		}
		return nil
	})
}

var namePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{2,63}$`)
var permissionPart = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

func validName(name string) bool { return namePattern.MatchString(name) }

func validPassword(password string) bool {
	// bcrypt ignores bytes after 72 in some implementations; reject them here
	// rather than accepting two distinct-looking strings as the same password.
	return len(password) >= 12 && len(password) <= 72 && strings.TrimSpace(password) != ""
}

func validNames(names []string) bool {
	// 重复角色不是另一个合法分配，直接拒绝比静默去重更容易发现调用错误
	// 上限控制关系表写入量，也限制单次认证时权限聚合的工作量
	if len(names) > 32 {
		return false
	}
	seen := make(map[string]bool, len(names))
	for _, name := range names {
		if !validName(name) || seen[name] {
			return false
		}
		seen[name] = true
	}
	return true
}

func validPermission(permission string) bool {
	// 允许 system:users:read 和 system:* 等稳定命名，不允许空段或空格
	// '*' 必须独占一段，其含义才与 Sa-Token v0.2.3 的匹配规则一致
	if len(permission) == 0 || len(permission) > 128 {
		return false
	}
	parts := strings.Split(permission, ":")
	if len(parts) > 8 {
		return false
	}
	for _, part := range parts {
		if part != "*" && !permissionPart.MatchString(part) {
			return false
		}
	}
	// Sa-Token v0.2.3 对结尾 :* 优先使用字面前缀匹配，不展开前面的 *
	// 拒绝无法按分段通配语义匹配的组合，避免成功保存后仍始终被拒绝
	if len(parts) > 1 && parts[len(parts)-1] == "*" {
		for _, part := range parts[:len(parts)-1] {
			if part == "*" {
				return false
			}
		}
	}
	return true
}

func contains(values []string, value string) bool {
	for _, item := range values {
		if item == value {
			return true
		}
	}
	return false
}

func safeDBError(err error) error {
	// SQL 驱动错误常含约束名和输入值，不能直接送给 HTTP 响应或日志
	// 只透出已定义类别，未知数据库问题统一成为服务不可用
	if err == nil {
		return nil
	}
	for _, category := range []error{ErrUnauthorized, ErrForbidden, ErrInvalidCredentials,
		ErrInvalidInput, ErrConflict, ErrNotFound} {
		if errors.Is(err, category) {
			return category
		}
	}
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return ErrNotFound
	}
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		return ErrConflict
	}
	return ErrUnavailable
}
