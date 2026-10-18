package db

import (
	"context"
	"time"

	"mochi-oa-server-go/internal/logger"
	"mochi-oa-server-go/internal/types"
)

// 本文件实现公司职位系统（jobs + company_members.position_id/position_extra）的 MySQL 访问。
// 默认职位清单在首次 job:list / job:create 时懒播种（该公司 jobs 为空才插，不重复）。

// defaultJobNames 默认职位清单（排序号=数组下标 0..N，名称逐字对齐用户原文）。
var defaultJobNames = []string{
	// ── 组1 管理决策层（15） ──
	"董事长", "副董事长", "总经理", "副总经理", "总裁", "副总裁", "董事", "监事",
	"首席执行官CEO", "首席运营官COO", "首席财务官CFO", "首席技术官CTO", "首席人力官CHRO", "首席产品官CPO", "首席市场官CMO",
	// ── 组2 行政人事类（16） ──
	"行政总监", "行政经理", "行政主管", "行政专员", "前台", "后勤专员", "档案管理员", "采购主管", "采购专员",
	"人事总监", "人事经理", "人事主管", "招聘专员", "薪酬绩效专员", "培训专员", "员工关系专员",
	// ── 组3 财务类（11） ──
	"财务总监", "财务经理", "会计主管", "总账会计", "成本会计", "税务会计", "应收会计", "应付会计", "出纳", "审计专员", "预算专员",
	// ── 组4 技术研发类（23） ──
	"技术总监", "研发经理", "技术主管", "架构师", "后端工程师", "前端工程师", "全栈工程师",
	"移动端开发工程师Android", "移动端开发工程师iOS",
	"测试经理", "测试工程师功能测试", "自动化测试工程师", "性能测试工程师",
	"运维经理", "运维工程师", "DevOps工程师", "数据库工程师DBA", "网络工程师", "安全工程师",
	"UI设计师", "UX交互设计师", "产品总监", "产品经理", "产品助理",
	// ── 组5 市场销售类（17） ──
	"市场总监", "市场经理", "品牌主管", "品牌专员", "新媒体运营", "活动策划", "推广专员",
	"销售总监", "销售经理", "销售主管", "销售代表/业务员", "销售助理",
	"客户总监", "客户经理", "客服主管", "在线客服", "售后专员",
	// ── 组6 生产/项目/供应链（14） ──
	"生产总监", "生产经理", "车间主管", "质检员QC", "品质主管QA", "仓库主管", "仓管员", "计划专员PMC",
	"项目总监", "项目经理", "项目助理", "造价工程师", "工程主管", "施工员",
	// ── 组7 法务风控（3） ──
	"法务总监", "法务专员", "风控专员",
	// ── 组8 通用基础职位（7） ──
	"部门主管", "部门助理", "专员", "实习生", "临时工", "顾问外部顾问", "自由岗位(自定义输入)",
}

func jobToItem(r types.JobRow) types.JobItem {
	return types.JobItem{
		ID:          r.ID,
		CompanyID:   r.CompanyID,
		Name:        r.Name,
		Description: r.Description,
		Enabled:     r.Enabled,
		SortOrder:   r.SortOrder,
		CreatedAt:   toIso(r.CreatedAt),
		UpdatedAt:   toIso(r.UpdatedAt),
	}
}

// EnsureDefaultJobs 该公司 jobs 为空时一次性播种默认清单；返回插入条数（0=已有数据，不重复）。
func (s *MySqlStore) EnsureDefaultJobs(ctx context.Context, companyID int64) (int64, error) {
	var cnt int64
	if err := s.gorm.WithContext(ctx).Table("jobs").Where("company_id = ?", companyID).Count(&cnt).Error; err != nil {
		return 0, err
	}
	if cnt > 0 {
		return 0, nil
	}
	now := time.Now().UTC()
	// 注意：JobRow.Enabled 是 bool，GORM 会把零值 false 当作未设置而走 DB DEFAULT=1，
	// 导致默认职位被插成 enabled=1。这里用 map 显式写 enabled=false。
	rows := make([]map[string]any, 0, len(defaultJobNames))
	for i, name := range defaultJobNames {
		desc := ""
		rows = append(rows, map[string]any{
			"company_id": companyID, "name": name, "description": &desc,
			"enabled": false, "sort_order": int64(i), "created_at": now, "updated_at": now,
		})
	}
	if err := s.gorm.WithContext(ctx).Table("jobs").Create(&rows).Error; err != nil {
		return 0, err
	}
	logger.Info("db/job", "默认职位清单播种完成", "companyId", companyID, "count", len(rows))
	return int64(len(rows)), nil
}

// CreateJob 新建职位（先 ensure 默认清单，让管理员在其上增改）。
// 默认 enabled=false（未启用），管理员在职位管理启用后成员才可被绑定。
// 注意：JobRow.Enabled 是 bool，GORM 把零值 false 当未设置走 DB DEFAULT=1，
// 所以必须用 map 显式写 enabled=false（与 EnsureDefaultJobs 同构）。
func (s *MySqlStore) CreateJob(ctx context.Context, in CreateJobInput) (*types.JobItem, error) {
	if _, err := s.EnsureDefaultJobs(ctx, in.CompanyID); err != nil {
		logger.Error("db/job", "job:create 播种默认清单失败", "companyId", in.CompanyID, "error", err.Error())
	}
	now := time.Now().UTC()
	row := map[string]any{
		"company_id":  in.CompanyID,
		"name":        in.Name,
		"description": in.Description,
		"enabled":     false, // 默认不启用
		"sort_order":  in.SortOrder,
		"created_at":  now,
		"updated_at":  now,
	}
	if err := s.gorm.WithContext(ctx).Table("jobs").Create(&row).Error; err != nil {
		logger.Error("db/job", "job:create 插入失败", "companyId", in.CompanyID, "name", in.Name, "error", err.Error())
		return nil, err
	}
	// 回查自增 ID（map insert 不回填 struct）。
	var created types.JobRow
	if err := s.gorm.WithContext(ctx).Table("jobs").
		Where("company_id = ? AND name = ? AND created_at = ?", in.CompanyID, in.Name, now).
		Order("id DESC").Take(&created).Error; err != nil {
		logger.Error("db/job", "job:create 回查失败", "companyId", in.CompanyID, "name", in.Name, "error", err.Error())
		return nil, err
	}
	it := jobToItem(created)
	return &it, nil
}

// UpdateJob 更新职位。
func (s *MySqlStore) UpdateJob(ctx context.Context, id int64, in UpdateJobInput) (*types.JobItem, error) {
	updates := map[string]any{"updated_at": time.Now().UTC()}
	if in.Name != nil {
		updates["name"] = *in.Name
	}
	if in.Description != nil {
		updates["description"] = *in.Description
	}
	if in.Enabled != nil {
		updates["enabled"] = *in.Enabled
	}
	if in.SortOrder != nil {
		updates["sort_order"] = *in.SortOrder
	}
	res := s.gorm.WithContext(ctx).Table("jobs").Where("id = ?", id).Updates(updates)
	if res.Error != nil {
		return nil, res.Error
	}
	if res.RowsAffected == 0 {
		return nil, &NotFoundError{Msg: "job not found"}
	}
	return s.getJobByID(ctx, id)
}

func (s *MySqlStore) getJobByID(ctx context.Context, id int64) (*types.JobItem, error) {
	var r types.JobRow
	err := s.gorm.WithContext(ctx).Table("jobs").Where("id = ?", id).Take(&r).Error
	if err != nil {
		if gormNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	it := jobToItem(r)
	return &it, nil
}

// DeleteJob 删职位；同时把绑定它的成员 position_id/position_extra 置空。
func (s *MySqlStore) DeleteJob(ctx context.Context, id int64) error {
	tx := s.gorm.WithContext(ctx).Begin()
	// 解绑成员（position_id 置空）。
	res := tx.Table("company_members").Where("position_id = ?", id).
		Updates(map[string]any{"position_id": nil, "position_extra": nil})
	if res.Error != nil {
		tx.Rollback()
		return res.Error
	}
	if res.RowsAffected > 0 {
		logger.Info("db/job", "删除职位并解绑成员", "jobId", id, "unboundMembers", res.RowsAffected)
	}
	res = tx.Table("jobs").Where("id = ?", id).Delete(&types.JobRow{})
	if res.Error != nil {
		tx.Rollback()
		return res.Error
	}
	if res.RowsAffected == 0 {
		tx.Rollback()
		return &NotFoundError{Msg: "job not found"}
	}
	return tx.Commit().Error
}

// ListJobs keyword 匹配 name/description；sort_order asc, id asc。
func (s *MySqlStore) ListJobs(ctx context.Context, companyID int64, keyword string, enabledOnly bool, page, pageSize int64) ([]types.JobItem, int64, error) {
	if _, err := s.EnsureDefaultJobs(ctx, companyID); err != nil {
		logger.Error("db/job", "job:list 播种默认清单失败", "companyId", companyID, "error", err.Error())
	}
	q := s.gorm.WithContext(ctx).Table("jobs").Where("company_id = ?", companyID)
	if keyword != "" {
		like := "%" + keyword + "%"
		q = q.Where("name LIKE ? OR description LIKE ?", like, like)
	}
	if enabledOnly {
		q = q.Where("enabled = ?", true)
	}
	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	if page <= 0 {
		page = 1
	}
	if pageSize <= 0 {
		pageSize = 20
	}
	if pageSize > 100 {
		pageSize = 100
	}
	rows := []types.JobRow{}
	if err := q.Order("sort_order ASC").Order("id ASC").
		Limit(int(pageSize)).Offset(int((page - 1) * pageSize)).Find(&rows).Error; err != nil {
		return nil, 0, err
	}
	out := []types.JobItem{}
	for _, r := range rows {
		out = append(out, jobToItem(r))
	}
	return out, total, nil
}

// BindPosition 设置成员职位绑定。jobID 为 nil 表示解绑。校验在 handler 做（成员/职位归属）。
func (s *MySqlStore) BindPosition(ctx context.Context, companyID, userID int64, jobID *int64, extra *string) error {
	res := s.gorm.WithContext(ctx).Table("company_members").
		Where("company_id = ?", companyID).Where("user_id = ?", userID).
		Updates(map[string]any{"position_id": jobID, "position_extra": extra})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return &NotFoundError{Msg: "member not found"}
	}
	return nil
}

// bindingScanRow 联表行：成员 + 用户昵称 + 部门 + 职位。
type bindingScanRow struct {
	UserID         int64   `gorm:"column:user_id"`
	Nick           string  `gorm:"column:nick"`
	Username       string  `gorm:"column:username"`
	DepartmentID   *int64  `gorm:"column:department_id"`
	DepartmentName *string `gorm:"column:department_name"`
	PositionID     *int64  `gorm:"column:position_id"`
	PositionName   *string `gorm:"column:position_name"`
	PositionExtra  *string `gorm:"column:position_extra"`
}

// ListBindings 公司全部成员（未绑定职位 positionId=null），联表部门与职位。
func (s *MySqlStore) ListBindings(ctx context.Context, companyID int64, page, pageSize int64) ([]types.BindingItem, int64, error) {
	base := s.gorm.WithContext(ctx).Table("company_members cm").
		Joins("LEFT JOIN users u ON u.id = cm.user_id").
		Joins("LEFT JOIN departments d ON d.id = (SELECT dm.department_id FROM department_members dm WHERE dm.user_id = cm.user_id LIMIT 1)").
		Joins("LEFT JOIN jobs j ON j.id = cm.position_id").
		Where("cm.company_id = ?", companyID)
	var total int64
	if err := base.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	if page <= 0 {
		page = 1
	}
	if pageSize <= 0 {
		pageSize = 20
	}
	if pageSize > 100 {
		pageSize = 100
	}
	rows := []bindingScanRow{}
	err := s.gorm.WithContext(ctx).Table("company_members cm").
		Joins("LEFT JOIN users u ON u.id = cm.user_id").
		Joins("LEFT JOIN departments d ON d.id = (SELECT dm.department_id FROM department_members dm WHERE dm.user_id = cm.user_id LIMIT 1)").
		Joins("LEFT JOIN jobs j ON j.id = cm.position_id").
		Select("cm.user_id, u.nick, u.username, d.id AS department_id, d.name AS department_name, cm.position_id, j.name AS position_name, cm.position_extra").
		Where("cm.company_id = ?", companyID).
		Order("cm.user_id ASC").
		Limit(int(pageSize)).Offset(int((page - 1) * pageSize)).
		Find(&rows).Error
	if err != nil {
		return nil, 0, err
	}
	out := []types.BindingItem{}
	for _, r := range rows {
		out = append(out, types.BindingItem{
			UserID: r.UserID, Nick: r.Nick, Username: r.Username,
			DepartmentID: r.DepartmentID, DepartmentName: r.DepartmentName,
			PositionID: r.PositionID, PositionName: r.PositionName, PositionExtra: r.PositionExtra,
		})
	}
	return out, total, nil
}
