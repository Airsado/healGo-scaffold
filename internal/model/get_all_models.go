// Package model 是 GORM 模型的集中登记入口，对应 gin-scaffold 的同名目录
package model

import (
	"healgo-scaffold/internal/auth"
	"healgo-scaffold/internal/model/note"
	"healgo-scaffold/internal/settings"
)

// GetAllModels 不执行 SQL新增业务模型时在这里显式登记，启动阶段统一迁移
// 演示模型单独开关，复制模板后不会因为普通启动就生成 example_notes 表
func GetAllModels(enableExamples bool) []any {
	// 系统能力的模型暂随其基础包维护，业务模型统一放在本目录的子包中
	models := append(auth.Models(), settings.Models()...)
	if enableExamples {
		models = append(models, &note.Note{})
	}
	return models
}
