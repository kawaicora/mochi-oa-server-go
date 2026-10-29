// Package db 的 MySqlStore 方法实现（拆分文件便于维护；与 mysql.go 同包）。
// 每个方法 1:1 对应 mysql-store.ts 的同名方法，语义、排序、过滤、转换均对齐。
//
// 本文件全部走 GORM（s.gorm），不再出现裸 SQL 字面量。
package db

import (
	"context"
	"strings"
	"time"

	"gorm.io/gorm"

	"mochi-oa-server-go/internal/types"
)

// ==================== 用户 ====================

// CreateUser 创建用户（extra 初始为 NULL）。
func (s *MySqlStore) CreateUser(ctx context.Context, in CreateUserInput) (*types.User, error) {
	nick := in.Nick
	if nick == "" {
		nick = ""
	}
	row := types.UserRow{
		Username:     in.Username,
		PasswordHash: in.PasswordHash,
		Nick:         nick,
		Avatar:       in.Avatar,
		Email:        in.Email,
		Phone:        in.Phone,
		Extra:        nil,
		CreatedAt:    time.Now().UTC(),
	}
	if err := s.gorm.WithContext(ctx).Table("users").Create(&row).Error; err != nil {
		if isUniqueErr(err) {
			return nil, &AlreadyExistsError{Msg: "username taken"}
		}
		return nil, err
	}
	return s.GetUserByID(ctx, row.ID)
}

// GetUserByUsername 按用户名取（含密码散列）。
func (s *MySqlStore) GetUserByUsername(ctx context.Context, username string) (*types.UserWithPassword, error) {
	var r types.UserRow
	err := s.gorm.WithContext(ctx).Table("users").Where("username = ?", username).Take(&r).Error
	if gormNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &types.UserWithPassword{User: rowToUser(&r), PasswordHash: r.PasswordHash}, nil
}

// GetUserByEmail 按邮箱取（含密码散列）。
func (s *MySqlStore) GetUserByEmail(ctx context.Context, email string) (*types.UserWithPassword, error) {
	var r types.UserRow
	err := s.gorm.WithContext(ctx).Table("users").Where("email = ?", email).Take(&r).Error
	if gormNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &types.UserWithPassword{User: rowToUser(&r), PasswordHash: r.PasswordHash}, nil
}

// GetUserByID 按主键取用户。
func (s *MySqlStore) GetUserByID(ctx context.Context, id int64) (*types.User, error) {
	var r types.UserRow
	err := s.gorm.WithContext(ctx).Table("users").Where("id = ?", id).Take(&r).Error
	if gormNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	u := rowToUser(&r)
	return &u, nil
}

// ListUsers 列全部用户（id 升序）。
func (s *MySqlStore) ListUsers(ctx context.Context) ([]types.User, error) {
	rows := []types.UserRow{}
	if err := s.gorm.WithContext(ctx).Table("users").Order("id ASC").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := []types.User{}
	for i := range rows {
		out = append(out, rowToUser(&rows[i]))
	}
	return out, nil
}

// UpdateUserProfile 更新资料（仅更新传入字段）。
func (s *MySqlStore) UpdateUserProfile(ctx context.Context, userID int64, patch UserProfilePatch) error {
	updates := map[string]any{}
	if patch.Nick != nil {
		updates["nick"] = *patch.Nick
	}
	if patch.Avatar != nil {
		updates["avatar"] = *patch.Avatar
	}
	if patch.Phone != nil {
		updates["phone"] = *patch.Phone
	}
	if patch.Email != nil {
		updates["email"] = *patch.Email
	}
	if patch.Extra != nil {
		updates["extra"] = *patch.Extra
	}
	if len(updates) == 0 {
		return nil
	}
	return s.gorm.WithContext(ctx).Table("users").Where("id = ?", userID).Updates(updates).Error
}

// UpdateUserPassword 更新密码散列。
func (s *MySqlStore) UpdateUserPassword(ctx context.Context, userID int64, passwordHash string) error {
	return s.gorm.WithContext(ctx).Table("users").
		Where("id = ?", userID).
		Updates(map[string]any{"password_hash": passwordHash}).Error
}

// ==================== 公司 ====================

// CreateCompany 创建公司，并把 owner 加入为 owner 成员。
func (s *MySqlStore) CreateCompany(ctx context.Context, in CreateCompanyInput) (*types.Company, error) {
	row := types.CompanyRow{Name: in.Name, Code: in.Code, OwnerID: in.OwnerID, CreatedAt: time.Now().UTC()}
	if err := s.gorm.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Table("companies").Create(&row).Error; err != nil {
			if isUniqueErr(err) {
				return &AlreadyExistsError{Msg: "code taken"}
			}
			return err
		}
		return tx.Table("company_members").Create(map[string]any{
			"company_id": row.ID, "user_id": in.OwnerID, "role": "owner", "joined_at": time.Now().UTC(),
		}).Error
	}); err != nil {
		return nil, err
	}
	return s.GetCompanyByID(ctx, row.ID)
}

// GetCompanyByID 按主键取公司。
func (s *MySqlStore) GetCompanyByID(ctx context.Context, id int64) (*types.Company, error) {
	var r types.CompanyRow
	err := s.gorm.WithContext(ctx).Table("companies").Where("id = ?", id).Take(&r).Error
	if gormNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	c := rowToCompany(&r)
	return &c, nil
}

// GetCompanyByCode 按加入码取公司。
func (s *MySqlStore) GetCompanyByCode(ctx context.Context, code string) (*types.Company, error) {
	var r types.CompanyRow
	err := s.gorm.WithContext(ctx).Table("companies").Where("code = ?", code).Take(&r).Error
	if gormNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	c := rowToCompany(&r)
	return &c, nil
}

// GetCompanyByName 按名称取公司。
func (s *MySqlStore) GetCompanyByName(ctx context.Context, name string) (*types.Company, error) {
	var r types.CompanyRow
	err := s.gorm.WithContext(ctx).Table("companies").Where("name = ?", name).Take(&r).Error
	if gormNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	c := rowToCompany(&r)
	return &c, nil
}

// ListCompanies 列公司（name LIKE 模糊，id 升序，上限 100）。
func (s *MySqlStore) ListCompanies(ctx context.Context, keyword string) ([]types.Company, error) {
	q := s.gorm.WithContext(ctx).Table("companies")
	if kw := strings.TrimSpace(keyword); kw != "" {
		q = q.Where("name LIKE ?", "%"+kw+"%")
	}
	rows := []types.CompanyRow{}
	if err := q.Order("id ASC").Limit(100).Find(&rows).Error; err != nil {
		return nil, err
	}
	out := []types.Company{}
	for i := range rows {
		out = append(out, rowToCompany(&rows[i]))
	}
	return out, nil
}

// AddMember 加入公司（重复 → AlreadyExists）。
func (s *MySqlStore) AddMember(ctx context.Context, companyID, userID int64, role string) error {
	err := s.gorm.WithContext(ctx).Table("company_members").Create(map[string]any{
		"company_id": companyID, "user_id": userID, "role": role, "joined_at": time.Now().UTC(),
	}).Error
	if err != nil {
		if isUniqueErr(err) {
			return &AlreadyExistsError{Msg: "already a member"}
		}
		return err
	}
	return nil
}

// RemoveMember 移除成员（不存在 → NotFound）。
func (s *MySqlStore) RemoveMember(ctx context.Context, companyID, userID int64) error {
	res := s.gorm.WithContext(ctx).Table("company_members").
		Where("company_id = ? AND user_id = ?", companyID, userID).
		Delete(nil)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return &NotFoundError{Msg: "not a member"}
	}
	return nil
}

// SetRole 设置成员角色（不存在 → NotFound）。
func (s *MySqlStore) SetRole(ctx context.Context, companyID, userID int64, role string) error {
	res := s.gorm.WithContext(ctx).Table("company_members").
		Where("company_id = ? AND user_id = ?", companyID, userID).
		Updates(map[string]any{"role": role})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return &NotFoundError{Msg: "not a member"}
	}
	return nil
}

// GetMemberRole 取成员角色（非成员返回空串+nil）。
func (s *MySqlStore) GetMemberRole(ctx context.Context, companyID, userID int64) (string, error) {
	var role string
	err := s.gorm.WithContext(ctx).Table("company_members").
		Select("role").
		Where("company_id = ? AND user_id = ?", companyID, userID).
		Take(&role).Error
	if gormNotFound(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return role, nil
}

// GetMembers 列公司成员（联用户，joined_at 升序）。
func (s *MySqlStore) GetMembers(ctx context.Context, companyID int64) ([]types.Member, error) {
	// joined_at 先扫成 time.Time，再统一走 toIso 输出 +08:00（禁止裸 time.Time/UTC 透传带 Z）。
	type memberRow struct {
		CompanyID int64     `gorm:"column:company_id"`
		UserID    int64     `gorm:"column:user_id"`
		Role      string    `gorm:"column:role"`
		JoinedAt  time.Time `gorm:"column:joined_at"`
		Username  string    `gorm:"column:username"`
		Nick      string    `gorm:"column:nick"`
	}
	rows := []memberRow{}
	if err := s.gorm.WithContext(ctx).Table("company_members m").
		Select("m.company_id, m.user_id, m.role, m.joined_at, u.username, u.nick").
		Joins("LEFT JOIN users u ON u.id = m.user_id").
		Where("m.company_id = ?", companyID).
		Order("m.joined_at ASC").
		Scan(&rows).Error; err != nil {
		return nil, err
	}
	out := []types.Member{}
	for _, r := range rows {
		un := r.Username
		nk := r.Nick
		m := types.Member{
			CompanyID: r.CompanyID,
			UserID:    r.UserID,
			Role:      r.Role,
			JoinedAt:  toIso(r.JoinedAt),
			Username:  &un,
			Nick:      &nk,
		}
		out = append(out, m)
	}
	return out, nil
}

// membershipRow GetUserCompanies 的联表临时行。
type membershipRow struct {
	ID        int64     `gorm:"column:id"`
	Name      string    `gorm:"column:name"`
	Code      string    `gorm:"column:code"`
	OwnerID   int64     `gorm:"column:owner_id"`
	Avatar    string    `gorm:"column:avatar"`
	CreatedAt time.Time `gorm:"column:created_at"`
	Role      string    `gorm:"column:role"`
}

// GetUserCompanies 取用户所属公司（联公司）。
func (s *MySqlStore) GetUserCompanies(ctx context.Context, userID int64) ([]types.Membership, error) {
	rows := []membershipRow{}
	if err := s.gorm.WithContext(ctx).Table("company_members m").
		Select("c.id, c.name, c.code, c.owner_id, c.avatar, c.created_at, m.role").
		Joins("JOIN companies c ON c.id = m.company_id").
		Where("m.user_id = ?", userID).
		Order("m.company_id ASC").
		Scan(&rows).Error; err != nil {
		return nil, err
	}
	out := []types.Membership{}
	for _, r := range rows {
		cr := types.CompanyRow{ID: r.ID, Name: r.Name, Code: r.Code, OwnerID: r.OwnerID, Avatar: r.Avatar, CreatedAt: r.CreatedAt}
		out = append(out, types.Membership{Company: rowToCompany(&cr), Role: r.Role})
	}
	return out, nil
}

// RenameCompany 重命名公司。
func (s *MySqlStore) RenameCompany(ctx context.Context, companyID int64, name string) error {
	res := s.gorm.WithContext(ctx).Table("companies").
		Where("id = ?", companyID).
		Updates(map[string]any{"name": name})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return &NotFoundError{Msg: "company not found"}
	}
	return nil
}

// UpdateCompanyLogo 更新公司 logo（avatar URL）。
func (s *MySqlStore) UpdateCompanyLogo(ctx context.Context, companyID int64, avatar string) error {
	return s.gorm.WithContext(ctx).Table("companies").
		Where("id = ?", companyID).
		Updates(map[string]any{"avatar": avatar}).Error
}

// SetCompanyOwner 转让公司（更新 owner_id）。
func (s *MySqlStore) SetCompanyOwner(ctx context.Context, companyID, ownerID int64) error {
	res := s.gorm.WithContext(ctx).Table("companies").
		Where("id = ?", companyID).
		Updates(map[string]any{"owner_id": ownerID})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return &NotFoundError{Msg: "company not found"}
	}
	return nil
}

// DeleteCompany 解散公司：删群、部门成员、部门、公司成员、邀请。
func (s *MySqlStore) DeleteCompany(ctx context.Context, companyID int64) error {
	groups, err := s.listGroupsRows(ctx, companyID)
	if err != nil {
		return err
	}
	for _, g := range groups {
		if err := s.DeleteGroup(ctx, g); err != nil {
			return err
		}
	}
	// 旧实现用 DELETE dm FROM department_members dm JOIN departments d ...：
	// 语义等价于「先取该公司全部部门 id，再按 department_id IN (...) 删除」。
	var deptIDs []int64
	if err := s.gorm.WithContext(ctx).Table("departments").
		Where("company_id = ?", companyID).Pluck("id", &deptIDs).Error; err != nil {
		return err
	}
	if len(deptIDs) > 0 {
		if err := s.gorm.WithContext(ctx).Table("department_members").
			Where("department_id IN ?", deptIDs).Delete(nil).Error; err != nil {
			return err
		}
	}
	if err := s.gorm.WithContext(ctx).Table("departments").
		Where("company_id = ?", companyID).Delete(nil).Error; err != nil {
		return err
	}
	if err := s.gorm.WithContext(ctx).Table("company_members").
		Where("company_id = ?", companyID).Delete(nil).Error; err != nil {
		return err
	}
	if err := s.gorm.WithContext(ctx).Table("company_invitations").
		Where("company_id = ?", companyID).Delete(nil).Error; err != nil {
		return err
	}
	return s.gorm.WithContext(ctx).Table("companies").Where("id = ?", companyID).Delete(nil).Error
}

// listGroupsRows 取某公司全部群 id（供 DeleteCompany 内部用）。
func (s *MySqlStore) listGroupsRows(ctx context.Context, companyID int64) ([]int64, error) {
	out := []int64{}
	if err := s.gorm.WithContext(ctx).Table("`groups`").
		Where("company_id = ?", companyID).Pluck("id", &out).Error; err != nil {
		return nil, err
	}
	return out, nil
}

// ==================== 部门 ====================

// CreateDepartment 创建部门。
func (s *MySqlStore) CreateDepartment(ctx context.Context, in CreateDepartmentInput) (*types.Department, error) {
	row := types.DepartmentRow{
		CompanyID: in.CompanyID, Name: in.Name, ParentID: in.ParentID, CreatedAt: time.Now().UTC(),
	}
	if err := s.gorm.WithContext(ctx).Table("departments").Create(&row).Error; err != nil {
		return nil, err
	}
	return s.GetDepartmentByID(ctx, row.ID)
}

// GetDepartmentByID 按主键取部门。
func (s *MySqlStore) GetDepartmentByID(ctx context.Context, id int64) (*types.Department, error) {
	var r types.DepartmentRow
	err := s.gorm.WithContext(ctx).Table("departments").Where("id = ?", id).Take(&r).Error
	if gormNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	d := rowToDepartment(&r)
	return &d, nil
}

// ListDepartments 列部门（id 升序）。
func (s *MySqlStore) ListDepartments(ctx context.Context, companyID int64) ([]types.Department, error) {
	rows := []types.DepartmentRow{}
	if err := s.gorm.WithContext(ctx).Table("departments").
		Where("company_id = ?", companyID).Order("id ASC").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := []types.Department{}
	for i := range rows {
		out = append(out, rowToDepartment(&rows[i]))
	}
	return out, nil
}

// AssignDepartment 把用户加入部门。
func (s *MySqlStore) AssignDepartment(ctx context.Context, departmentID, userID int64) error {
	err := s.gorm.WithContext(ctx).Table("department_members").Create(map[string]any{
		"department_id": departmentID, "user_id": userID, "joined_at": time.Now().UTC(),
	}).Error
	if err != nil {
		if isUniqueErr(err) {
			return &AlreadyExistsError{Msg: "already in department"}
		}
		return err
	}
	return nil
}

// RemoveDepartmentMember 移除部门成员。
func (s *MySqlStore) RemoveDepartmentMember(ctx context.Context, departmentID, userID int64) error {
	res := s.gorm.WithContext(ctx).Table("department_members").
		Where("department_id = ? AND user_id = ?", departmentID, userID).
		Delete(nil)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return &NotFoundError{Msg: "not in department"}
	}
	return nil
}

// GetDepartmentMembers 列部门成员（联用户）。
func (s *MySqlStore) GetDepartmentMembers(ctx context.Context, departmentID int64) ([]types.DepartmentMember, error) {
	type deptRow struct {
		DepartmentID int64     `gorm:"column:department_id"`
		UserID       int64     `gorm:"column:user_id"`
		JoinedAt     time.Time `gorm:"column:joined_at"`
		Username     string    `gorm:"column:username"`
		Nick         string    `gorm:"column:nick"`
	}
	rows := []deptRow{}
	if err := s.gorm.WithContext(ctx).Table("department_members m").
		Select("m.department_id, m.user_id, m.joined_at, u.username, u.nick").
		Joins("LEFT JOIN users u ON u.id = m.user_id").
		Where("m.department_id = ?", departmentID).
		Order("m.joined_at ASC").
		Scan(&rows).Error; err != nil {
		return nil, err
	}
	out := []types.DepartmentMember{}
	for _, r := range rows {
		un := r.Username
		nk := r.Nick
		out = append(out, types.DepartmentMember{
			DepartmentID: r.DepartmentID,
			UserID:       r.UserID,
			JoinedAt:     toIso(r.JoinedAt),
			Username:     &un,
			Nick:         &nk,
		})
	}
	return out, nil
}

// DeleteDepartment 删除部门并级联移除成员。
func (s *MySqlStore) DeleteDepartment(ctx context.Context, departmentID int64) error {
	if err := s.gorm.WithContext(ctx).Table("department_members").
		Where("department_id = ?", departmentID).Delete(nil).Error; err != nil {
		return err
	}
	res := s.gorm.WithContext(ctx).Table("departments").Where("id = ?", departmentID).Delete(nil)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return &NotFoundError{Msg: "department not found"}
	}
	return nil
}

// ==================== 群 ====================

// CreateGroup 创建群（companyId<=0 存 NULL），owner 自动入群。
func (s *MySqlStore) CreateGroup(ctx context.Context, in CreateGroupInput) (*types.Group, error) {
	var companyID *int64
	if in.CompanyID > 0 {
		v := in.CompanyID
		companyID = &v
	}
	row := types.GroupRow{
		CompanyID: companyID, DepartmentID: in.DepartmentID, Name: in.Name,
		Code: in.Code, OwnerID: in.OwnerID, CreatedAt: time.Now().UTC(),
	}
	if err := s.gorm.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Table("`groups`").Create(&row).Error; err != nil {
			if isUniqueErr(err) {
				return &AlreadyExistsError{Msg: "code taken"}
			}
			return err
		}
		return tx.Table("group_members").Create(map[string]any{
			"group_id": row.ID, "user_id": in.OwnerID, "role": "owner", "joined_at": time.Now().UTC(),
		}).Error
	}); err != nil {
		return nil, err
	}
	return s.GetGroupByID(ctx, row.ID)
}

// GetGroupByID 按主键取群。
func (s *MySqlStore) GetGroupByID(ctx context.Context, id int64) (*types.Group, error) {
	var r types.GroupRow
	err := s.gorm.WithContext(ctx).Table("`groups`").Where("id = ?", id).Take(&r).Error
	if gormNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	g := rowToGroup(&r)
	return &g, nil
}

// GetGroupByName 按公司+名称取群（companyId<=0 视为 NULL）。
func (s *MySqlStore) GetGroupByName(ctx context.Context, companyID int64, name string) (*types.Group, error) {
	var r types.GroupRow
	var err error
	if companyID > 0 {
		err = s.gorm.WithContext(ctx).Table("`groups`").
			Where("name = ? AND company_id = ?", name, companyID).Take(&r).Error
	} else {
		err = s.gorm.WithContext(ctx).Table("`groups`").
			Where("name = ? AND company_id IS NULL", name).Take(&r).Error
	}
	if gormNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	g := rowToGroup(&r)
	return &g, nil
}

// GetGroupByCode 按加入码取群。
func (s *MySqlStore) GetGroupByCode(ctx context.Context, code string) (*types.Group, error) {
	var r types.GroupRow
	err := s.gorm.WithContext(ctx).Table("`groups`").Where("code = ?", code).Take(&r).Error
	if gormNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	g := rowToGroup(&r)
	return &g, nil
}

// ListGroups 列群（可选 companyId/keyword，id 升序，上限 100）。
func (s *MySqlStore) ListGroups(ctx context.Context, companyID int64, keyword string) ([]types.Group, error) {
	q := s.gorm.WithContext(ctx).Table("`groups`")
	if companyID != 0 {
		q = q.Where("company_id = ?", companyID)
	}
	if kw := strings.TrimSpace(keyword); kw != "" {
		q = q.Where("name LIKE ?", "%"+kw+"%")
	}
	rows := []types.GroupRow{}
	if err := q.Order("id ASC").Limit(100).Find(&rows).Error; err != nil {
		return nil, err
	}
	out := []types.Group{}
	for i := range rows {
		out = append(out, rowToGroup(&rows[i]))
	}
	return out, nil
}

// AddGroupMember 入群。
func (s *MySqlStore) AddGroupMember(ctx context.Context, groupID, userID int64, role string) error {
	err := s.gorm.WithContext(ctx).Table("group_members").Create(map[string]any{
		"group_id": groupID, "user_id": userID, "role": role, "joined_at": time.Now().UTC(),
	}).Error
	if err != nil {
		if isUniqueErr(err) {
			return &AlreadyExistsError{Msg: "already in group"}
		}
		return err
	}
	return nil
}

// RemoveGroupMember 移出群。
func (s *MySqlStore) RemoveGroupMember(ctx context.Context, groupID, userID int64) error {
	res := s.gorm.WithContext(ctx).Table("group_members").
		Where("group_id = ? AND user_id = ?", groupID, userID).
		Delete(nil)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return &NotFoundError{Msg: "not in group"}
	}
	return nil
}

// GetGroupMemberRole 取群成员角色。
func (s *MySqlStore) GetGroupMemberRole(ctx context.Context, groupID, userID int64) (string, error) {
	var role string
	err := s.gorm.WithContext(ctx).Table("group_members").
		Select("role").
		Where("group_id = ? AND user_id = ?", groupID, userID).
		Take(&role).Error
	if gormNotFound(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return role, nil
}

// GetGroupMember 取群成员（含禁言截止，ISO 字符串）。
func (s *MySqlStore) GetGroupMember(ctx context.Context, groupID, userID int64) (string, *string, error) {
	var r struct {
		Role       string     `gorm:"column:role"`
		MutedUntil *time.Time `gorm:"column:muted_until"`
	}
	err := s.gorm.WithContext(ctx).Table("group_members").
		Select("role, muted_until").
		Where("group_id = ? AND user_id = ?", groupID, userID).
		Take(&r).Error
	if gormNotFound(err) {
		return "", nil, nil
	}
	if err != nil {
		return "", nil, err
	}
	var mutedPtr *string
	if r.MutedUntil != nil && !r.MutedUntil.IsZero() {
		s := toIso(*r.MutedUntil)
		mutedPtr = &s
	}
	return r.Role, mutedPtr, nil
}

// SetGroupMemberRole 设置群成员角色。
func (s *MySqlStore) SetGroupMemberRole(ctx context.Context, groupID, userID int64, role string) error {
	return s.gorm.WithContext(ctx).Table("group_members").
		Where("group_id = ? AND user_id = ?", groupID, userID).
		Updates(map[string]any{"role": role}).Error
}

// SetGroupMemberMuted 设置禁言截止（nil 清除）。
func (s *MySqlStore) SetGroupMemberMuted(ctx context.Context, groupID, userID int64, mutedUntil *string) error {
	var val any
	if mutedUntil != nil && *mutedUntil != "" {
		t, err := parseTimeFlex(*mutedUntil)
		if err != nil {
			return err
		}
		val = t
	}
	return s.gorm.WithContext(ctx).Table("group_members").
		Where("group_id = ? AND user_id = ?", groupID, userID).
		Updates(map[string]any{"muted_until": val}).Error
}

// SetGroupOwner 转让群主。
func (s *MySqlStore) SetGroupOwner(ctx context.Context, groupID, ownerID int64) error {
	res := s.gorm.WithContext(ctx).Table("`groups`").
		Where("id = ?", groupID).
		Updates(map[string]any{"owner_id": ownerID})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return &NotFoundError{Msg: "group not found"}
	}
	return nil
}

// DeleteGroup 解散群：删群会话消息/成员，删群成员。
func (s *MySqlStore) DeleteGroup(ctx context.Context, groupID int64) error {
	convIDs := []int64{}
	if err := s.gorm.WithContext(ctx).Table("conversations").
		Where("type = 'group' AND group_id = ?", groupID).Pluck("id", &convIDs).Error; err != nil {
		return err
	}
	for _, cid := range convIDs {
		if err := s.gorm.WithContext(ctx).Table("messages").
			Where("conversation_id = ?", cid).Delete(nil).Error; err != nil {
			return err
		}
		if err := s.gorm.WithContext(ctx).Table("conversation_members").
			Where("conversation_id = ?", cid).Delete(nil).Error; err != nil {
			return err
		}
		if err := s.gorm.WithContext(ctx).Table("conversations").
			Where("id = ?", cid).Delete(nil).Error; err != nil {
			return err
		}
	}
	if err := s.gorm.WithContext(ctx).Table("group_members").
		Where("group_id = ?", groupID).Delete(nil).Error; err != nil {
		return err
	}
	return s.gorm.WithContext(ctx).Table("`groups`").Where("id = ?", groupID).Delete(nil).Error
}

// groupMemberListRow GetGroupMembers 的联表临时行。
type groupMemberListRow struct {
	GroupID    int64      `gorm:"column:group_id"`
	UserID     int64      `gorm:"column:user_id"`
	Role       string     `gorm:"column:role"`
	JoinedAt   time.Time  `gorm:"column:joined_at"`
	MutedUntil *time.Time `gorm:"column:muted_until"`
	Username   *string    `gorm:"column:username"`
	Nick       *string    `gorm:"column:nick"`
}

// GetGroupMembers 列群成员（联用户）。
func (s *MySqlStore) GetGroupMembers(ctx context.Context, groupID int64) ([]types.GroupMember, error) {
	rows := []groupMemberListRow{}
	if err := s.gorm.WithContext(ctx).Table("group_members m").
		Select("m.group_id, m.user_id, m.role, m.joined_at, m.muted_until, u.username, u.nick").
		Joins("LEFT JOIN users u ON u.id = m.user_id").
		Where("m.group_id = ?", groupID).
		Order("m.joined_at ASC").
		Scan(&rows).Error; err != nil {
		return nil, err
	}
	out := []types.GroupMember{}
	for _, r := range rows {
		m := types.GroupMember{
			GroupID: r.GroupID, UserID: r.UserID, Role: r.Role, JoinedAt: toIso(r.JoinedAt),
			Username: r.Username, Nick: r.Nick,
		}
		if r.MutedUntil != nil && !r.MutedUntil.IsZero() {
			iso := toIso(*r.MutedUntil)
			m.MutedUntil = &iso
		}
		out = append(out, m)
	}
	return out, nil
}

// GetUserGroups 取用户所在群。
func (s *MySqlStore) GetUserGroups(ctx context.Context, userID int64) ([]types.Group, error) {
	rows := []types.GroupRow{}
	if err := s.gorm.WithContext(ctx).Table("group_members m").
		Select("g.id, g.company_id, g.department_id, g.name, g.code, g.owner_id, g.created_at").
		Joins("JOIN `groups` g ON g.id = m.group_id").
		Where("m.user_id = ?", userID).
		Order("m.group_id ASC").
		Scan(&rows).Error; err != nil {
		return nil, err
	}
	out := []types.Group{}
	for i := range rows {
		out = append(out, rowToGroup(&rows[i]))
	}
	return out, nil
}
