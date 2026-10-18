package db

import (
	"context"
	"strconv"
	"time"

	"gorm.io/gorm"

	"mochi-oa-server-go/internal/types"
)

// 本文件实现考勤申请（补卡/请假/调休）与公司级审批流配置的 MySQL 访问。
// 风格照 mysql_methods.go / report.go：s.gorm.WithContext(ctx).Table("显式表名")，
// 联表用自定义 scan struct + gorm:"column:xxx"，统一转换函数输出 JSON 视图。

// attendanceScanRow 联表行：考勤申请字段 + 发起人 nick/username。
type attendanceScanRow struct {
	ID            int64      `gorm:"column:id"`
	CompanyID     int64      `gorm:"column:company_id"`
	UserID        int64      `gorm:"column:user_id"`
	Type          string     `gorm:"column:type"`
	Status        string     `gorm:"column:status"`
	PunchTime     *string    `gorm:"column:punch_time"`
	LeaveType     *string    `gorm:"column:leave_type"`
	CompType      *string    `gorm:"column:comp_type"`
	StartTime     *time.Time `gorm:"column:start_time"`
	EndTime       *time.Time `gorm:"column:end_time"`
	DurationHours *float64   `gorm:"column:duration_hours"`
	Reason        string     `gorm:"column:reason"`
	ApproverID    *int64     `gorm:"column:approver_id"`
	ApprovalNote  *string    `gorm:"column:approval_note"`
	ApprovedAt    *time.Time `gorm:"column:approved_at"`
	ApproverIDs   string     `gorm:"column:approver_ids"`
	CcIDs         string     `gorm:"column:cc_ids"`
	CcViewedIDs   string     `gorm:"column:cc_viewed_ids"`
	CreatedAt     time.Time  `gorm:"column:created_at"`
	UpdatedAt     time.Time  `gorm:"column:updated_at"`
	Username      string     `gorm:"column:username"`
	Nick          string     `gorm:"column:nick"`
}

// attendanceBaseQuery 带 LEFT JOIN users 的基础查询。
func (s *MySqlStore) attendanceBaseQuery(ctx context.Context) *gorm.DB {
	return s.gorm.WithContext(ctx).Table("attendance_requests r").
		Joins("LEFT JOIN users u ON u.id = r.user_id")
}

// scanRowToAttendance 联表行 → JSON 视图；JSON 数组列经 decodeRecipients 兜底 []。
func scanRowToAttendance(r attendanceScanRow) types.AttendanceRequest {
	return types.AttendanceRequest{
		ID:            r.ID,
		CompanyID:     r.CompanyID,
		UserID:        r.UserID,
		Type:          r.Type,
		Status:        r.Status,
		PunchTime:     r.PunchTime,
		LeaveType:     r.LeaveType,
		CompType:      r.CompType,
		StartTime:     isoPtr(r.StartTime),
		EndTime:       isoPtr(r.EndTime),
		DurationHours: r.DurationHours,
		Reason:        r.Reason,
		ApproverID:    r.ApproverID,
		ApprovalNote:  r.ApprovalNote,
		ApprovedAt:    isoPtr(r.ApprovedAt),
		ApproverIDs:   decodeRecipients(r.ApproverIDs),
		CcIDs:         decodeRecipients(r.CcIDs),
		CcViewedIds:   decodeRecipients(r.CcViewedIDs),
		CreatedAt:     toIso(r.CreatedAt),
		UpdatedAt:     toIso(r.UpdatedAt),
		Nick:          r.Nick,
		Username:      r.Username,
	}
}

// attendanceSelect 联表查询统一列清单（含 approver_ids/cc_ids）。
const attendanceSelect = "r.id, r.company_id, r.user_id, r.type, r.status, r.punch_time, r.leave_type, r.comp_type, r.start_time, r.end_time, r.duration_hours, r.reason, r.approver_id, r.approval_note, r.approved_at, r.approver_ids, r.cc_ids, r.cc_viewed_ids, r.created_at, r.updated_at, u.username, u.nick"

// CreateAttendanceRequest 发起考勤申请；审批人/抄送人由调用方按流程快照传入。
func (s *MySqlStore) CreateAttendanceRequest(ctx context.Context, in CreateAttendanceRequestInput) (*types.AttendanceRequest, error) {
	now := time.Now().UTC()
	row := types.AttendanceRequestRow{
		CompanyID:     in.CompanyID,
		UserID:        in.UserID,
		Type:          in.Type,
		PunchTime:     in.PunchTime,
		LeaveType:     in.LeaveType,
		CompType:      in.CompType,
		StartTime:     in.StartTime,
		EndTime:       in.EndTime,
		DurationHours: in.DurationHours,
		Reason:        in.Reason,
		ApproverIDs:   encodeRecipients(in.ApproverIDs),
		CcIDs:         encodeRecipients(in.CcIDs),
		ApproverID:    nil,
		ApprovalNote:  nil,
		ApprovedAt:    nil,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	if err := s.gorm.WithContext(ctx).Table("attendance_requests").Create(&row).Error; err != nil {
		return nil, err
	}
	return s.GetAttendanceRequestByID(ctx, row.ID)
}

// GetAttendanceRequestByID 按 id 取单条（联表补 nick/username）。
func (s *MySqlStore) GetAttendanceRequestByID(ctx context.Context, id int64) (*types.AttendanceRequest, error) {
	var r attendanceScanRow
	err := s.attendanceBaseQuery(ctx).Select(attendanceSelect).Where("r.id = ?", id).Take(&r).Error
	if err != nil {
		if gormNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	out := scanRowToAttendance(r)
	return &out, nil
}

// ListAttendanceRequestsByUser 只看本人历史；type 空串=不限；createdAt 倒序分页。
// dateStart/dateEnd（YYYY-MM-DD 含两端）非空时：DATE(start_time) 落在区间内才命中
// （日历紫色点用；NULL start_time 的补卡等不参与区间过滤）。
func (s *MySqlStore) ListAttendanceRequestsByUser(ctx context.Context, companyID, userID int64, typ, dateStart, dateEnd string, page, pageSize int64) ([]types.AttendanceRequest, int64, error) {
	q := s.gorm.WithContext(ctx).Table("attendance_requests r").
		Where("r.company_id = ?", companyID).
		Where("r.user_id = ?", userID)
	if typ != "" {
		q = q.Where("r.type = ?", typ)
	}
	if dateStart != "" {
		q = q.Where("DATE(r.start_time) >= ?", dateStart)
	}
	if dateEnd != "" {
		q = q.Where("DATE(r.start_time) <= ?", dateEnd)
	}
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
	rows := []attendanceScanRow{}
	listQ := s.attendanceBaseQuery(ctx).Select(attendanceSelect).
		Where("r.company_id = ?", companyID).
		Where("r.user_id = ?", userID)
	if typ != "" {
		listQ = listQ.Where("r.type = ?", typ)
	}
	if dateStart != "" {
		listQ = listQ.Where("DATE(r.start_time) >= ?", dateStart)
	}
	if dateEnd != "" {
		listQ = listQ.Where("DATE(r.start_time) <= ?", dateEnd)
	}
	if err := listQ.Order("r.created_at DESC, r.id DESC").
		Limit(int(pageSize)).
		Offset(int(offset)).
		Find(&rows).Error; err != nil {
		return nil, 0, err
	}
	out := []types.AttendanceRequest{}
	for _, r := range rows {
		out = append(out, scanRowToAttendance(r))
	}
	return out, total, nil
}

// ApproveAttendanceRequest 置 approved：approver_id=审批人、approved_at=now。
func (s *MySqlStore) ApproveAttendanceRequest(ctx context.Context, id, approverID int64) (*types.AttendanceRequest, error) {
	now := time.Now().UTC()
	res := s.gorm.WithContext(ctx).Table("attendance_requests").
		Where("id = ?", id).
		Updates(map[string]any{
			"status":      "approved",
			"approver_id": approverID,
			"approved_at": now,
			"updated_at":  now,
		})
	if res.Error != nil {
		return nil, res.Error
	}
	if res.RowsAffected == 0 {
		return nil, &NotFoundError{Msg: "attendance request not found"}
	}
	return s.GetAttendanceRequestByID(ctx, id)
}

// RejectAttendanceRequest 置 rejected：approval_note=驳回理由。
func (s *MySqlStore) RejectAttendanceRequest(ctx context.Context, id int64, note string) (*types.AttendanceRequest, error) {
	now := time.Now().UTC()
	res := s.gorm.WithContext(ctx).Table("attendance_requests").
		Where("id = ?", id).
		Updates(map[string]any{
			"status":        "rejected",
			"approval_note": note,
			"updated_at":    now,
		})
	if res.Error != nil {
		return nil, res.Error
	}
	if res.RowsAffected == 0 {
		return nil, &NotFoundError{Msg: "attendance request not found"}
	}
	return s.GetAttendanceRequestByID(ctx, id)
}

// ListReceivedAttendance 收到审批：本公司内 本人提交 OR 本人被列为审批人 的全部申请。
// 口径含自审批（本人提交且自己审批）。handler 按 status 拆 pending/processed。
func (s *MySqlStore) ListReceivedAttendance(ctx context.Context, companyID, userID int64) ([]types.AttendanceRequest, error) {
	approverJSON := strconv.FormatInt(userID, 10)
	rows := []attendanceScanRow{}
	err := s.attendanceBaseQuery(ctx).Select(attendanceSelect).
		Where("r.company_id = ?", companyID).
		Where("(r.user_id = ? OR JSON_CONTAINS(r.approver_ids, ?))", userID, approverJSON).
		Order("r.created_at DESC, r.id DESC").
		Find(&rows).Error
	if err != nil {
		return nil, err
	}
	out := []types.AttendanceRequest{}
	for _, r := range rows {
		out = append(out, scanRowToAttendance(r))
	}
	return out, nil
}

// ListReceivedCC 收到抄送：本公司内 本人提交 OR 本人被列为抄送人 的全部申请。
// 每条已带 cc_viewed_ids，handler 据此计算 viewed。
func (s *MySqlStore) ListReceivedCC(ctx context.Context, companyID, userID int64) ([]types.AttendanceRequest, error) {
	ccJSON := strconv.FormatInt(userID, 10)
	rows := []attendanceScanRow{}
	err := s.attendanceBaseQuery(ctx).Select(attendanceSelect).
		Where("r.company_id = ?", companyID).
		Where("(r.user_id = ? OR JSON_CONTAINS(r.cc_ids, ?))", userID, ccJSON).
		Order("r.created_at DESC, r.id DESC").
		Find(&rows).Error
	if err != nil {
		return nil, err
	}
	out := []types.AttendanceRequest{}
	for _, r := range rows {
		out = append(out, scanRowToAttendance(r))
	}
	return out, nil
}

// MarkCCViewed 把 userID 加入该申请的 cc_viewed_ids（幂等，已存在则不重复追加）。
func (s *MySqlStore) MarkCCViewed(ctx context.Context, requestID, userID int64) error {
	// 先读当前已查看数组（用显式结构接收，避免 Scan 到裸 string 被 GORM 丢弃）。
	var row struct {
		CcViewedIDs string `gorm:"column:cc_viewed_ids"`
	}
	err := s.gorm.WithContext(ctx).Table("attendance_requests").
		Where("id = ?", requestID).
		Select("IFNULL(cc_viewed_ids,'[]') AS cc_viewed_ids").
		Take(&row).Error
	if err != nil {
		return err
	}
	existing := decodeRecipients(row.CcViewedIDs)
	for _, v := range existing {
		if v == userID {
			return nil // 幂等：已查看过
		}
	}
	existing = append(existing, userID)
	return s.gorm.WithContext(ctx).Table("attendance_requests").
		Where("id = ?", requestID).
		Updates(map[string]any{
			"cc_viewed_ids": encodeRecipients(existing),
			"updated_at":    time.Now().UTC(),
		}).Error
}
