// Package mapper 是笔记模块唯一使用 GORM 执行 SQL 的层
// 所有资源查询把 owner 放进 SQL，不能先按 ID 读取再依赖控制器过滤
package mapper

import (
	"context"
	"errors"
	"time"

	"heago-scaffold/internal/db"
	model "heago-scaffold/internal/model/note"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// 每个实例只绑定一个 DB 或 tx；方法不重新从全局取连接，事务才不会中途逃逸
type NoteMapper struct {
	database *gorm.DB
	reader   *db.Cluster
}

// NewNoteMapper 只绑定连接，不查询或迁移；装配入口可能在表初始化前调用它
// database 也可以是事务 tx，多表用例必须把同一个 tx 传给所有参与的 mapper
func NewNoteMapper(database *gorm.DB) (*NoteMapper, error) {
	if database == nil {
		return nil, model.ErrInvalidInput
	}
	return &NoteMapper{database: database}, nil
}

// NewNoteMapperWithReader 只让 List 使用显式副本入口；其他方法固定主库
// 事务内使用上面的 NewNoteMapper(tx)，防止列表查询逃出当前事务
func NewNoteMapperWithReader(cluster *db.Cluster) (*NoteMapper, error) {
	if cluster == nil || cluster.Primary == nil {
		return nil, model.ErrInvalidInput
	}
	return &NoteMapper{database: cluster.Primary, reader: cluster}, nil
}

func (m *NoteMapper) Create(ctx context.Context, note model.Note) (model.Note, error) {
	// ID 与版本由持久化入口固定，调用者不能通过“创建”覆盖已有资源
	note.ID = 0
	note.Revision = 1
	if err := m.database.WithContext(ctx).Create(&note).Error; err != nil {
		return model.Note{}, safeError(ctx, err)
	}
	return note, nil
}

func (m *NoteMapper) Get(ctx context.Context, ownerID, id uint64) (model.Note, error) {
	var note model.Note
	// 参数化查询将数据和 SQL 结构分开；ID 和 owner 不通过字符串拼接进入查询
	err := m.database.WithContext(ctx).Where("owner_id = ? AND id = ?", ownerID, id).First(&note).Error
	if err != nil {
		return model.Note{}, safeError(ctx, err)
	}
	return note, nil
}

func (m *NoteMapper) List(ctx context.Context, ownerID uint64, offset, limit int) ([]model.Note, int64, error) {
	// mapper 也限制窗口，避免绕过服务的内部调用把负数解释成取消 LIMIT
	if offset < 0 || offset > 1000000 || limit < 1 || limit > 100 {
		return nil, 0, model.ErrInvalidInput
	}
	var total int64
	items := make([]model.Note, 0)
	read := func(database *gorm.DB) error {
		// 整个分页操作一起回退，清除前一次的部分结果，避免累计/混用两个节点
		items, total = make([]model.Note, 0), 0
		if err := database.Model(&model.Note{}).Where("owner_id = ?", ownerID).Count(&total).Error; err != nil {
			return err
		}
		// 单副本保证两条查询落同一节点；并发增删时仍不承诺同一个快照
		return database.Where("owner_id = ?", ownerID).Order("id DESC").Offset(offset).Limit(limit).Find(&items).Error
	}
	var err error
	if m.reader != nil {
		err = m.reader.Read(ctx, read)
	} else {
		err = read(m.database.WithContext(ctx))
	}
	if err != nil {
		return nil, 0, safeError(ctx, err)
	}
	return items, total, nil
}

func (m *NoteMapper) Update(ctx context.Context, ownerID, id, revision uint64, title, content string) (model.Note, error) {
	if m.database.Dialector.Name() == "mysql" {
		return m.updateMySQL(ctx, ownerID, id, revision, title, content)
	}
	var updated model.Note
	// map 明确包含空字符串，避免 GORM struct Updates 忽略零值，导致内容无法清空
	// 比较版本与增加版本在同一 SQL 中完成，不能“先读版本再无条件更新”
	// UpdatedAt 由服务端设置，不允许客户端伪造编辑时间参与并发判断
	result := m.database.WithContext(ctx).Model(&updated).Clauses(clause.Returning{}).
		Where("owner_id = ? AND id = ? AND revision = ?", ownerID, id, revision).
		Updates(map[string]any{"title": title, "content": content, "revision": gorm.Expr("revision + 1"), "updated_at": time.Now().UTC()})
	if result.Error != nil {
		return model.Note{}, safeError(ctx, result.Error)
	}
	// 无匹配行既可能是旧版本，也可能是资源不可见；不要把两者都当成更新成功
	if result.RowsAffected == 0 {
		return model.Note{}, m.missingOrConflict(ctx, ownerID, id)
	}
	// RETURNING 给出本次写入结果，避免再读时意外返回其他并发请求的新版本
	return updated, nil
}

func (m *NoteMapper) updateMySQL(ctx context.Context, ownerID, id, revision uint64, title, content string) (model.Note, error) {
	var updated model.Note
	// MySQL 无 UPDATE RETURNING写入和回读必须在同一事务，保持行锁直到提交，
	// 避免返回另一并发写者的版本；嵌套事务会沿用调用方 tx 和 savepoint
	err := m.database.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		result := tx.Model(&model.Note{}).Where("owner_id = ? AND id = ? AND revision = ?", ownerID, id, revision).
			Updates(map[string]any{"title": title, "content": content, "revision": gorm.Expr("revision + 1"), "updated_at": time.Now().UTC()})
		if result.Error != nil {
			return safeError(ctx, result.Error)
		}
		// 锁定读属于 current read，不会被 MySQL REPEATABLE READ 的旧快照误导
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("owner_id = ? AND id = ?", ownerID, id).First(&updated).Error
		if err != nil {
			return safeError(ctx, err)
		}
		if result.RowsAffected == 0 {
			return model.ErrConflict
		}
		return nil
	})
	if err != nil {
		// Begin/Commit 也可能失败；驱动错误不能绕过闭包的脱敏边界
		if errors.Is(err, model.ErrConflict) || errors.Is(err, model.ErrNotFound) || errors.Is(err, model.ErrUnavailable) {
			return model.Note{}, err
		}
		return model.Note{}, safeError(ctx, err)
	}
	return updated, nil
}

func (m *NoteMapper) Delete(ctx context.Context, ownerID, id, revision uint64) error {
	// 删除同样需要客户端版本，避免用户基于旧页面删除刚被修改过的内容
	result := m.database.WithContext(ctx).Where("owner_id = ? AND id = ? AND revision = ?", ownerID, id, revision).Delete(&model.Note{})
	if result.Error != nil {
		return safeError(ctx, result.Error)
	}
	if result.RowsAffected == 0 {
		return m.missingOrConflict(ctx, ownerID, id)
	}
	return nil
}

func (m *NoteMapper) missingOrConflict(ctx context.Context, ownerID, id uint64) error {
	// 只有当前 owner 的行存在时才报告冲突，不泄漏另一个账号是否拥有该 ID
	// 这个分类读取不参与写入决策，真正的并发正确性已由上一条 CAS SQL 保证
	if m.database.Dialector.Name() == "mysql" {
		// 外层 tx 可能已有旧快照；分类也用 current read，不能把已删除的行报成409
		var current model.Note
		if err := m.database.WithContext(ctx).Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("owner_id = ? AND id = ?", ownerID, id).First(&current).Error; err != nil {
			return safeError(ctx, err)
		}
		return model.ErrConflict
	}
	if _, err := m.Get(ctx, ownerID, id); err != nil {
		return err
	}
	return model.ErrConflict
}

func safeError(ctx context.Context, err error) error {
	// 保留取消分类便于上层停止重试，同时不透出驱动携带的 SQL 或原始输入
	// errors.Join 仍允许调用者同时用 errors.Is 判断存储故障与 context 取消
	if ctx.Err() != nil {
		return errors.Join(model.ErrUnavailable, ctx.Err())
	}
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return model.ErrNotFound
	}
	// 唯一性由数据库约束最终决定，先查询“是否重名”无法避免并发重复创建
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		return model.ErrConflict
	}
	// 接受没有启用 TranslateError 的 GORM 实例，也不需要依赖具体 pgx 错误类型
	var state interface{ SQLState() string }
	if errors.As(err, &state) && state.SQLState() == "23505" {
		return model.ErrConflict
	}
	// 即使驱动错误包含 SQL、连接地址或用户正文，跨层只返回固定类别
	return model.ErrUnavailable
}
