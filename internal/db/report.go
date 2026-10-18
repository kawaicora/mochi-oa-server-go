// Package db 的日报/周报（reports / report_templates）MySqlStore 方法实现。
//
// 风格与 mysql_methods.go 一致：全部走 s.gorm，Table("...") 显式指定表名；
// 报告查询一律 LEFT JOIN users 补 nick/username（联表写法同 GetMembers）。
//
// 数据口径：
//   - periodDate 是 "YYYY-MM-DD" 字符串：日报=当天，周报=本周周一；
//   - 联合唯一索引 idx_report_period(company_id,user_id,type,period_date) 保证一人一周期一条，
//     CreateReport 撞唯一键时映射为 AlreadyExistsError（handler 文案「该周期的日报已存在」）；
//   - Sections 列落库为 JSON 字符串数组，读出时解码为 []string。
package db

import (
	"context"
	"encoding/json"
	"time"

	"gorm.io/gorm"

	"mochi-oa-server-go/internal/types"
)

// reportScanRow 是报告查询的联表扫描行：reports 全列 + users.username/nick。
// （GORM Scan 需要显式 column 标签，不能直接嵌 Row struct。）
type reportScanRow struct {
	ID           int64      `gorm:"column:id"`
	CompanyID    int64      `gorm:"column:company_id"`
	UserID       int64      `gorm:"column:user_id"`
	Type         string     `gorm:"column:type"`
	PeriodDate   string     `gorm:"column:period_date"`
	Title        string     `gorm:"column:title"`
	Content      string     `gorm:"column:content"`
	TemplateID   int64      `gorm:"column:template_id"`
	RecipientIDs string     `gorm:"column:recipient_ids"` // JSON 数组字符串，如 "[1,2]"
	Submitted    bool       `gorm:"column:submitted"`
	SubmittedAt  *time.Time `gorm:"column:submitted_at"`
	CreatedAt    time.Time  `gorm:"column:created_at"`
	UpdatedAt    time.Time  `gorm:"column:updated_at"`
	Username     string     `gorm:"column:username"`
	Nick         string     `gorm:"column:nick"`
}

// reportBaseQuery 构造报告联表查询（reports r LEFT JOIN users u），各查询复用。
func (s *MySqlStore) reportBaseQuery(ctx context.Context) *gorm.DB {
	return s.gorm.WithContext(ctx).Table("reports r").
		Select("r.id, r.company_id, r.user_id, r.type, r.period_date, r.title, r.content, r.template_id, r.recipient_ids, r.submitted, r.submitted_at, r.created_at, r.updated_at, u.username, u.nick").
		Joins("LEFT JOIN users u ON u.id = r.user_id")
}

// scanRowToReport 联表行 → 对外 ReportItem。
func scanRowToReport(r reportScanRow) types.ReportItem {
	return types.ReportItem{
		ID:           r.ID,
		CompanyID:    r.CompanyID,
		UserID:       r.UserID,
		Type:         r.Type,
		PeriodDate:   r.PeriodDate,
		Title:        r.Title,
		Content:      r.Content,
		TemplateID:   r.TemplateID,
		RecipientIds: decodeRecipients(r.RecipientIDs),
		Submitted:    r.Submitted,
		SubmittedAt:  isoPtr(r.SubmittedAt),
		CreatedAt:    toIso(r.CreatedAt),
		UpdatedAt:    toIso(r.UpdatedAt),
		Username:     r.Username,
		Nick:         r.Nick,
	}
}

// encodeSections 分节数组 → JSON 字符串（空数组落 "[]"）。
func encodeSections(sections []string) string {
	if len(sections) == 0 {
		return "[]"
	}
	b, err := json.Marshal(sections)
	if err != nil {
		return "[]"
	}
	return string(b)
}

// decodeSections JSON 字符串 → 分节数组（损坏时回退空数组，绝不 nil 给客户端）。
func decodeSections(raw string) []string {
	out := []string{}
	if raw == "" {
		return out
	}
	_ = json.Unmarshal([]byte(raw), &out)
	return out
}

// encodeRecipients 接收人 user_id 数组 → JSON 字符串（空落 "[]"）。
func encodeRecipients(ids []int64) string {
	if len(ids) == 0 {
		return "[]"
	}
	b, err := json.Marshal(ids)
	if err != nil {
		return "[]"
	}
	return string(b)
}

// decodeRecipients JSON 字符串 → 接收人数组（空串/损坏回退空数组，绝不 nil）。
func decodeRecipients(raw string) []int64 {
	out := []int64{}
	if raw == "" {
		return out
	}
	_ = json.Unmarshal([]byte(raw), &out)
	return out
}

// encodeStringArray 字符串数组 → JSON 字符串（空数组落 "[]"）。
func encodeStringArray(vals []string) string {
	if len(vals) == 0 {
		return "[]"
	}
	b, err := json.Marshal(vals)
	if err != nil {
		return "[]"
	}
	return string(b)
}

// decodeStringArray JSON 字符串 → 字符串数组（空串/损坏回退空数组，绝不 nil）。
func decodeStringArray(raw string) []string {
	out := []string{}
	if raw == "" {
		return out
	}
	_ = json.Unmarshal([]byte(raw), &out)
	return out
}

// ==================== 报告模板 ====================

// CreateReportTemplate 创建模板（admin/owner 在 handler 侧把关）。
func (s *MySqlStore) CreateReportTemplate(ctx context.Context, in CreateReportTemplateInput) (*types.ReportTemplate, error) {
	now := time.Now().UTC()
	row := types.ReportTemplateRow{
		CompanyID:   in.CompanyID,
		Name:        in.Name,
		Description: in.Description,
		Sections:    encodeSections(in.Sections),
		CreatedBy:   in.CreatedBy,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := s.gorm.WithContext(ctx).Table("report_templates").Create(&row).Error; err != nil {
		return nil, err
	}
	return &types.ReportTemplate{
		ID: row.ID, CompanyID: row.CompanyID, Name: row.Name, Description: row.Description,
		Sections: decodeSections(row.Sections), CreatedBy: row.CreatedBy,
		CreatedAt: toIso(row.CreatedAt), UpdatedAt: toIso(row.UpdatedAt),
	}, nil
}

// UpdateReportTemplate 更新模板（nil 字段不更新；Sections 非 nil 整体覆盖）。
func (s *MySqlStore) UpdateReportTemplate(ctx context.Context, id int64, in UpdateReportTemplateInput) (*types.ReportTemplate, error) {
	updates := map[string]any{"updated_at": time.Now().UTC()}
	if in.Name != nil {
		updates["name"] = *in.Name
	}
	if in.Description != nil {
		updates["description"] = *in.Description
	}
	if in.Sections != nil {
		updates["sections"] = encodeSections(in.Sections)
	}
	if len(updates) == 1 { // 只有 updated_at，没有业务字段变化
		return s.getReportTemplateByID(ctx, id)
	}
	res := s.gorm.WithContext(ctx).Table("report_templates").Where("id = ?", id).Updates(updates)
	if res.Error != nil {
		return nil, res.Error
	}
	if res.RowsAffected == 0 {
		return nil, &NotFoundError{Msg: "template not found"}
	}
	return s.getReportTemplateByID(ctx, id)
}

// DeleteReportTemplate 删除模板（不存在 → NotFound）。
func (s *MySqlStore) DeleteReportTemplate(ctx context.Context, id int64) error {
	res := s.gorm.WithContext(ctx).Table("report_templates").Where("id = ?", id).Delete(nil)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return &NotFoundError{Msg: "template not found"}
	}
	return nil
}

// getReportTemplateByID 内部按主键取模板（handler 校验归属时复用）。
func (s *MySqlStore) getReportTemplateByID(ctx context.Context, id int64) (*types.ReportTemplate, error) {
	var r types.ReportTemplateRow
	err := s.gorm.WithContext(ctx).Table("report_templates").Where("id = ?", id).Take(&r).Error
	if gormNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &types.ReportTemplate{
		ID: r.ID, CompanyID: r.CompanyID, Name: r.Name, Description: r.Description,
		Sections: decodeSections(r.Sections), CreatedBy: r.CreatedBy,
		CreatedAt: toIso(r.CreatedAt), UpdatedAt: toIso(r.UpdatedAt),
	}, nil
}

// GetReportTemplateByID 按主键取模板（对外）。
func (s *MySqlStore) GetReportTemplateByID(ctx context.Context, id int64) (*types.ReportTemplate, error) {
	return s.getReportTemplateByID(ctx, id)
}

// ListReportTemplates 列公司模板（id 升序）。
func (s *MySqlStore) ListReportTemplates(ctx context.Context, companyID int64) ([]types.ReportTemplate, error) {
	rows := []types.ReportTemplateRow{}
	if err := s.gorm.WithContext(ctx).Table("report_templates").
		Where("company_id = ?", companyID).Order("id ASC").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := []types.ReportTemplate{}
	for _, r := range rows {
		out = append(out, types.ReportTemplate{
			ID: r.ID, CompanyID: r.CompanyID, Name: r.Name, Description: r.Description,
			Sections: decodeSections(r.Sections), CreatedBy: r.CreatedBy,
			CreatedAt: toIso(r.CreatedAt), UpdatedAt: toIso(r.UpdatedAt),
		})
	}
	return out, nil
}

// ==================== 日报/周报 ====================

// CreateReport 创建报告；撞联合唯一键 → AlreadyExistsError（「该周期已存在」）。
func (s *MySqlStore) CreateReport(ctx context.Context, in CreateReportInput) (*types.ReportItem, error) {
	now := time.Now().UTC()
	row := types.ReportRow{
		CompanyID:    in.CompanyID,
		UserID:       in.UserID,
		Type:         in.Type,
		PeriodDate:   in.PeriodDate,
		Title:        in.Title,
		Content:      in.Content,
		TemplateID:   in.TemplateID,
		RecipientIDs: encodeRecipients(in.Recipients),
		Submitted:    false,
		SubmittedAt:  nil,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	if err := s.gorm.WithContext(ctx).Table("reports").Create(&row).Error; err != nil {
		if isUniqueErr(err) {
			return nil, &AlreadyExistsError{Msg: "report period already exists"}
		}
		return nil, err
	}
	return s.GetReportByID(ctx, row.ID)
}

// GetReportByID 按主键取报告（含联表昵称）；不存在返回 (nil, nil)。
func (s *MySqlStore) GetReportByID(ctx context.Context, id int64) (*types.ReportItem, error) {
	var r reportScanRow
	err := s.reportBaseQuery(ctx).Where("r.id = ?", id).Take(&r).Error
	if gormNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	item := scanRowToReport(r)
	return &item, nil
}

// GetReportByPeriod 按 (公司,用户,类型,周期) 取报告；不存在返回 (nil, nil)。
func (s *MySqlStore) GetReportByPeriod(ctx context.Context, companyID, userID int64, typ, periodDate string) (*types.ReportItem, error) {
	var r reportScanRow
	err := s.reportBaseQuery(ctx).
		Where("r.company_id = ? AND r.user_id = ? AND r.type = ? AND r.period_date = ?", companyID, userID, typ, periodDate).
		Take(&r).Error
	if gormNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	item := scanRowToReport(r)
	return &item, nil
}

// UpdateReport 更新报告字段（nil 不更新；submitted/submittedAt 不动）。
func (s *MySqlStore) UpdateReport(ctx context.Context, id int64, in UpdateReportInput) (*types.ReportItem, error) {
	updates := map[string]any{"updated_at": time.Now().UTC()}
	if in.Title != nil {
		updates["title"] = *in.Title
	}
	if in.Content != nil {
		updates["content"] = *in.Content
	}
	if in.TemplateID != nil {
		updates["template_id"] = *in.TemplateID
	}
	// Recipients 非 nil 即整体替换（传空切片=清空接收人）。
	if in.Recipients != nil {
		updates["recipient_ids"] = encodeRecipients(in.Recipients)
	}
	res := s.gorm.WithContext(ctx).Table("reports").Where("id = ?", id).Updates(updates)
	if res.Error != nil {
		return nil, res.Error
	}
	if res.RowsAffected == 0 {
		return nil, &NotFoundError{Msg: "report not found"}
	}
	return s.GetReportByID(ctx, id)
}

// SubmitReport 提交报告：置 submitted=true、submitted_at=now（UTC）。
func (s *MySqlStore) SubmitReport(ctx context.Context, id int64) (*types.ReportItem, error) {
	res := s.gorm.WithContext(ctx).Table("reports").
		Where("id = ?", id).
		Updates(map[string]any{"submitted": true, "submitted_at": time.Now().UTC(), "updated_at": time.Now().UTC()})
	if res.Error != nil {
		return nil, res.Error
	}
	if res.RowsAffected == 0 {
		return nil, &NotFoundError{Msg: "report not found"}
	}
	return s.GetReportByID(ctx, id)
}

// DeleteReport 删除报告（不存在 → NotFound）。
func (s *MySqlStore) DeleteReport(ctx context.Context, id int64) error {
	res := s.gorm.WithContext(ctx).Table("reports").Where("id = ?", id).Delete(nil)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return &NotFoundError{Msg: "report not found"}
	}
	return nil
}

// ListReports 公司+筛选+分页：先 count 再取页行；排序 id 倒序（新报告在前）。
func (s *MySqlStore) ListReports(ctx context.Context, in ListReportsInput) ([]types.ReportItem, int64, error) {
	q := s.gorm.WithContext(ctx).Table("reports r").Where("r.company_id = ?", in.CompanyID)
	if in.Type != "" {
		q = q.Where("r.type = ?", in.Type)
	}
	if in.UserID > 0 {
		q = q.Where("r.user_id = ?", in.UserID)
	}
	if in.PeriodStart != "" {
		q = q.Where("r.period_date >= ?", in.PeriodStart)
	}
	if in.PeriodEnd != "" {
		q = q.Where("r.period_date <= ?", in.PeriodEnd)
	}
	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	page := in.Page
	if page <= 0 {
		page = 1
	}
	pageSize := in.PageSize
	if pageSize <= 0 {
		pageSize = 20
	}
	if pageSize > 100 {
		pageSize = 100
	}
	offset := (page - 1) * pageSize
	rows := []reportScanRow{}
	err := q.Select("r.id, r.company_id, r.user_id, r.type, r.period_date, r.title, r.content, r.template_id, r.recipient_ids, r.submitted, r.submitted_at, r.created_at, r.updated_at, u.username, u.nick").
		Joins("LEFT JOIN users u ON u.id = r.user_id").
		Order("r.id DESC").
		Limit(int(pageSize)).
		Offset(int(offset)).
		Find(&rows).Error
	if err != nil {
		return nil, 0, err
	}
	out := []types.ReportItem{}
	for _, r := range rows {
		out = append(out, scanRowToReport(r))
	}
	return out, total, nil
}

// ListReportsByPeriod 统计用：一次取 [periodStart,periodEnd] 含两端的全部报告；
// userIDs 非空时只取这些成员（空=全公司）。排序 period_date 升序、id 升序。
func (s *MySqlStore) ListReportsByPeriod(ctx context.Context, companyID int64, typ, periodStart, periodEnd string, userIDs []int64) ([]types.ReportItem, error) {
	q := s.reportBaseQuery(ctx).Where("r.company_id = ?", companyID)
	if typ != "" {
		q = q.Where("r.type = ?", typ)
	}
	if periodStart != "" {
		q = q.Where("r.period_date >= ?", periodStart)
	}
	if periodEnd != "" {
		q = q.Where("r.period_date <= ?", periodEnd)
	}
	if len(userIDs) > 0 {
		q = q.Where("r.user_id IN ?", userIDs)
	}
	rows := []reportScanRow{}
	if err := q.Order("r.period_date ASC, r.id ASC").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := []types.ReportItem{}
	for _, r := range rows {
		out = append(out, scanRowToReport(r))
	}
	return out, nil
}

// ListInboxReports 收件箱：查「别人发给我」的报告——即 recipient_ids 这个 JSON 数组里包含本人 userId。
//
// JSON_CONTAINS(recipient_ids, CAST(? AS JSON))：
//   - recipient_ids 是 JSON 列，形如 "[1,2]"；
//   - 第二参数必须是合法 JSON 值；直接绑定数字时 MySQL JSON 数字匹配不能按字符串传，
//     用 CAST(? AS JSON) 把绑定的 int64 转成 JSON 值（数字 5 → JSON 5），再做数组包含判断；
//   - 命中即该报告把本人列进了接收人。
//
// createdAt 倒序（新收到的在前），分页返回（行, 总数）。
func (s *MySqlStore) ListInboxReports(ctx context.Context, companyID, userID int64, page, pageSize int64) ([]types.ReportItem, int64, error) {
	q := s.gorm.WithContext(ctx).Table("reports r").
		Where("r.company_id = ?", companyID).
		Where("JSON_CONTAINS(r.recipient_ids, CAST(? AS JSON))", userID)
	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	if page <= 0 {
		page = 1
	}
	if pageSize <= 0 {
		pageSize = 10
	}
	if pageSize > 100 {
		pageSize = 100
	}
	offset := (page - 1) * pageSize
	rows := []reportScanRow{}
	err := q.Select("r.id, r.company_id, r.user_id, r.type, r.period_date, r.title, r.content, r.template_id, r.recipient_ids, r.submitted, r.submitted_at, r.created_at, r.updated_at, u.username, u.nick").
		Joins("LEFT JOIN users u ON u.id = r.user_id").
		Order("r.created_at DESC, r.id DESC").
		Limit(int(pageSize)).
		Offset(int(offset)).
		Find(&rows).Error
	if err != nil {
		return nil, 0, err
	}
	out := []types.ReportItem{}
	for _, r := range rows {
		out = append(out, scanRowToReport(r))
	}
	return out, total, nil
}
