package auth

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"heago-scaffold/configs"

	"github.com/google/uuid"
	"github.com/sa-tokens/sa-token-go/core"
	"github.com/sa-tokens/sa-token-go/storage/memory"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
)

// Public errors are deliberately stable and contain neither SQL, password nor
// token details. Controllers map these categories to HTTP without leaking inputs.
var (
	ErrUnauthorized       = errors.New("unauthorized")
	ErrForbidden          = errors.New("forbidden")
	ErrInvalidCredentials = errors.New("invalid credentials")
	ErrUnavailable        = errors.New("authentication unavailable")
	ErrInvalidInput       = errors.New("invalid authentication input")
	ErrConflict           = errors.New("identity already exists")
	ErrNotFound           = errors.New("identity not found")
)

type Service struct {
	db      *gorm.DB
	manager *core.Manager
	close   func() error

	// Sa-Token persists account-session documents with read/modify/write calls.
	// One service mutex makes login, logout and permission sync atomic relative
	// to each other; separate DB operations remain concurrent and cancellable.
	mu sync.Mutex
}

func New(db *gorm.DB, cfg configs.Auth) (*Service, error) {
	// 生命周期由组合根控制：构造不查表，建表后才执行种子和查询，停机时 Close
	// 这里不调用全局默认 manager，测试实例和不同应用不会串会话
	// Sa-Token 使用整数秒，拒绝小数秒配置避免实际 TTL 与文件声明不同
	if db == nil || cfg.SessionTTL < time.Second || cfg.SessionTTL%time.Second != 0 || cfg.MaxSessions < 1 || cfg.MaxSessions > 100 {
		return nil, ErrInvalidInput
	}
	storage := memory.NewStorage()
	manager, err := core.NewBuilder().Storage(storage).TokenName("Authorization").
		Timeout(int64(cfg.SessionTTL / time.Second)).ActiveTimeout(-1).AutoRenew(false).
		TokenStyle(core.TokenStyleRandom64).IsShare(false).IsConcurrent(true).
		MaxLoginCount(cfg.MaxSessions).IsLog(false).IsPrintBanner(false).TryBuild()
	// This adapter intentionally selects memory storage: a process restart ends
	// sessions; multiple replicas need a shared store before enabling deployment.
	closer := storage.(interface{ Close() error }).Close
	if err != nil {
		_ = closer()
		return nil, ErrUnavailable
	}
	return &Service{db: db, manager: manager, close: closer}, nil
}

func (s *Service) Close() error { return s.close() }

// Login 是密码认证边界；路由层还应在调用前限制密码尝试频率
// 参数只用于校验与参数化查询，错误不会拼接任何原始输入
// 成功返回随机 token，token 本身不包含角色、密码或可解码的用户资料
func (s *Service) Login(ctx context.Context, username, password string) (string, Principal, error) {
	if !validName(username) || !validPassword(password) {
		return "", Principal{}, ErrInvalidCredentials
	}
	var user User
	// 数据库不可用与凭据错误必须区分，否则故障会被误报为密码输错
	// 对外仍只返回稳定错误类别，实际 SQL 细节不经过这个服务泄露
	err := s.db.WithContext(ctx).Where("username = ?", username).First(&user).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return "", Principal{}, ErrUnavailable
	}
	// Unknown and disabled accounts still perform a bcrypt comparison. This
	// reduces the obvious timing difference without exposing account existence.
	hash := user.PasswordHash
	if hash == "" {
		hash = dummyHash
	}
	passwordErr := bcrypt.CompareHashAndPassword([]byte(hash), []byte(password))
	if err != nil || !user.Enabled || passwordErr != nil {
		return "", Principal{}, ErrInvalidCredentials
	}
	p, err := s.principal(ctx, user.ID)
	if err != nil {
		return "", Principal{}, err
	}
	// A password change racing with login must not grant a token for the new
	// password version after verifying the old hash. The old version is safe:
	// Authenticate rejects it against the persisted AuthVersion on every call.
	if !p.Enabled || p.authVersion != user.AuthVersion {
		return "", Principal{}, ErrInvalidCredentials
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// 只对内存会话读改写加锁，耗时 bcrypt 与 SQL 不占用这把锁
	// 上限按同一个登录身份计算，避免同账号无限遗留活跃 token
	// v0.2.3 的账号索引按 device 保存，每次都用默认值会只索引最新 token
	// 内部独立 device 让库能枚举所有会话，MaxLoginCount 才真正约束并发
	// device 不接受客户端指定，避免伪造相同设备破坏独立会话和撤销语义
	device, err := uuid.NewRandom()
	if err != nil {
		return "", Principal{}, ErrUnavailable
	}
	token, err := s.manager.Login(loginID(p), device.String())
	if err != nil {
		return "", Principal{}, ErrUnavailable
	}
	return token, p, nil
}

func (s *Service) Authenticate(ctx context.Context, request *http.Request) (Principal, string, error) {
	// Authenticate 只证明当前请求是谁，不自动授予某个业务操作权限
	// 路由必须继续调用 RequirePermission 或 RequireRole 执行授权
	// token 不从 Cookie 或 query 回退读取，减少代理日志泄露和隐式认证
	token, ok := bearer(request)
	if !ok {
		return Principal{}, "", ErrUnauthorized
	}
	s.mu.Lock()
	id, err := s.manager.GetLoginID(token)
	s.mu.Unlock()
	if err != nil {
		return Principal{}, "", ErrUnauthorized
	}
	parts := strings.Split(id, ":")
	// 身份是内部生成的 ID:版本，不相信 token 对应的数据天然正确
	// 仍校验格式与正整数，存储损坏或异常数据按未登录处理
	if len(parts) != 2 {
		return Principal{}, "", ErrUnauthorized
	}
	userID, err := strconv.ParseUint(parts[0], 10, 64)
	if err != nil || userID == 0 {
		return Principal{}, "", ErrUnauthorized
	}
	p, err := s.principal(ctx, userID)
	if errors.Is(err, ErrNotFound) {
		return Principal{}, "", ErrUnauthorized
	}
	if err != nil {
		return Principal{}, "", err
	}
	// Role changes, account disabling and password changes take effect from the
	// database immediately; an old in-memory session never grants stale access.
	if !p.Enabled || loginID(p) != id {
		return Principal{}, "", ErrUnauthorized
	}
	return p, token, nil
}

func (s *Service) Logout(token string) error {
	// 退出撤销当前 token，其他浏览器独立登录不会被顺带注销
	// 修改密码的全会话撤销由持久化 AuthVersion 负责，两者语义不同
	// Sa-Token 的错误可能带内部 token 信息，因此只返回统一错误
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.manager.LogoutByToken(token); err != nil {
		return ErrUnauthorized
	}
	return nil
}

func (s *Service) RequirePermission(ctx context.Context, p Principal, permission string) error {
	// 权限名由服务端路由声明，不能把客户端自报的权限列表直接传入检查
	// 每次从数据库刷新权限，因此角色调整不需要等旧 token 自然到期
	// 原则是数据库失败即拒绝，不依赖过期缓存继续放行
	if !validPermission(permission) {
		return ErrInvalidInput
	}
	fresh, err := s.authorizable(ctx, p)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// 同账号并发请求可能加载不同时间点的权限列表
	// 将写入和检查合成临界区，避免检查到另一个请求刚写入的列表
	// Use Sa-Token's wildcard semantics rather than a second home-grown matcher.
	// Never mutate its process-global StpInterface; each Service owns its manager.
	if err := s.manager.SetPermissions(loginID(fresh), fresh.Permissions); err != nil {
		return ErrUnavailable
	}
	if !s.manager.HasPermission(loginID(fresh), permission) {
		return ErrForbidden
	}
	return nil
}

func (s *Service) RequireRole(ctx context.Context, p Principal, role string) error {
	// 角色检查适合“必须属于运维组”这类约束；普通功能建议声明权限
	// 两种接口都调用 Sa-Token，新增业务无需了解其底层会话存储格式
	// 单独复制 Principal 的 JSON 并不能通过检查，因为内部版本不会导出
	if !validName(role) {
		return ErrInvalidInput
	}
	fresh, err := s.authorizable(ctx, p)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.manager.SetRoles(loginID(fresh), fresh.Roles); err != nil {
		return ErrUnavailable
	}
	if !s.manager.HasRole(loginID(fresh), role) {
		return ErrForbidden
	}
	return nil
}

// Rechecking the version also protects authorization calls held by a request
// whose password was changed after its authentication middleware completed.
func (s *Service) authorizable(ctx context.Context, p Principal) (Principal, error) {
	// 不能只校验 Principal.ID：调用期间账号可能已被禁用或修改密码
	// 比较认证时拿到的版本与当前版本，使跨请求的旧身份也会失效
	fresh, err := s.principal(ctx, p.ID)
	if errors.Is(err, ErrNotFound) {
		return Principal{}, ErrUnauthorized
	}
	if err != nil {
		return Principal{}, err
	}
	if !fresh.Enabled || fresh.authVersion != p.authVersion {
		return Principal{}, ErrUnauthorized
	}
	return fresh, nil
}

func loginID(p Principal) string {
	// 版本放入 Sa-Token 身份而非公开 token，旧会话即使仍存在也无权限
	// strconv 避免自行拼接数字格式造成前导零等不同身份表示
	return strconv.FormatUint(p.ID, 10) + ":" + strconv.FormatUint(p.authVersion, 10)
}

// Header-only extraction avoids putting tokens into URLs, access logs or
// ambient cookies. Duplicate/merged headers are rejected, not guessed apart.
func bearer(request *http.Request) (string, bool) {
	// Header.Values 可识别重复字段；合并后的逗号在下面统一拒绝
	// TrimSpace 会把畸形凭据“修好”，因此这里明确拒绝任何内部空白
	values := request.Header.Values("Authorization")
	if len(values) != 1 || !strings.HasPrefix(values[0], "Bearer ") {
		return "", false
	}
	token := strings.TrimPrefix(values[0], "Bearer ")
	return token, token != "" && !strings.ContainsAny(token, " \t\r\n,")
}

// A constant bcrypt hash avoids generating costly random hashes per unknown
// username. Its value is not a usable account or a seed administrator password.
const dummyHash = "$2a$10$7EqJtq98hPqEX7fNZaFWoO5hwm2T8Y7P1jx7yBd2xXlV4/1wl.wqG"
