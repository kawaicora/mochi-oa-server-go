// Package db 的 MySqlStore 方法实现（拆分文件便于维护；与 mysql.go 同包）。
// 每个方法 1:1 对应 mysql-store.ts 的同名方法，SQL 语义、排序、过滤、转换均对齐。
package db

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"mochi-oa-server-go/internal/types"
)

// ==================== 用户 ====================

// CreateUser 创建用户（extra 初始为 NULL）。
func (s *MySqlStore) CreateUser(ctx context.Context, in CreateUserInput) (*types.User, error) {
	nick, email, avatar, phone := in.Nick, in.Email, in.Avatar, in.Phone
	if nick == "" {
		nick = ""
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO users (username, password_hash, nick, avatar, email, phone, extra, created_at) VALUES (?,?,?,?,?,?,NULL, NOW(3))`,
		in.Username, in.PasswordHash, nick, avatar, email, phone)
	if err != nil {
		if isUniqueErr(err) {
			return nil, &AlreadyExistsError{Msg: "username taken"}
		}
		return nil, err
	}
	id, _ := res.LastInsertId()
	return s.GetUserByID(ctx, id)
}

// GetUserByUsername 按用户名取（含密码散列）。
func (s *MySqlStore) GetUserByUsername(ctx context.Context, username string) (*types.UserWithPassword, error) {
	r, err := scanUser(s.db.QueryRowContext(ctx,
		`SELECT id, username, password_hash, nick, avatar, email, phone, extra, session_days, created_at FROM users WHERE username=?`, username))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &types.UserWithPassword{User: rowToUser(r), PasswordHash: r.PasswordHash}, nil
}

// GetUserByEmail 按邮箱取（含密码散列）。
func (s *MySqlStore) GetUserByEmail(ctx context.Context, email string) (*types.UserWithPassword, error) {
	r, err := scanUser(s.db.QueryRowContext(ctx,
		`SELECT id, username, password_hash, nick, avatar, email, phone, extra, session_days, created_at FROM users WHERE email=?`, email))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &types.UserWithPassword{User: rowToUser(r), PasswordHash: r.PasswordHash}, nil
}

// GetUserByID 按主键取用户。
func (s *MySqlStore) GetUserByID(ctx context.Context, id int64) (*types.User, error) {
	r, err := scanUser(s.db.QueryRowContext(ctx,
		`SELECT id, username, password_hash, nick, avatar, email, phone, extra, session_days, created_at FROM users WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	u := rowToUser(r)
	return &u, nil
}

// ListUsers 列全部用户（id 升序）。
func (s *MySqlStore) ListUsers(ctx context.Context) ([]types.User, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, username, password_hash, nick, avatar, email, phone, extra, session_days, created_at FROM users ORDER BY id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []types.User{}
	for rows.Next() {
		r, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rowToUser(r))
	}
	return out, rows.Err()
}

// UpdateUserProfile 更新资料（仅更新传入字段）。
func (s *MySqlStore) UpdateUserProfile(ctx context.Context, userID int64, patch UserProfilePatch) error {
	sets, args := []string{}, []any{}
	if patch.Nick != nil {
		sets, args = append(sets, "nick=?"), append(args, *patch.Nick)
	}
	if patch.Avatar != nil {
		sets, args = append(sets, "avatar=?"), append(args, *patch.Avatar)
	}
	if patch.Phone != nil {
		sets, args = append(sets, "phone=?"), append(args, *patch.Phone)
	}
	if patch.Email != nil {
		sets, args = append(sets, "email=?"), append(args, *patch.Email)
	}
	if patch.Extra != nil {
		sets, args = append(sets, "extra=?"), append(args, *patch.Extra)
	}
	if len(sets) == 0 {
		return nil
	}
	args = append(args, userID)
	_, err := s.db.ExecContext(ctx, `UPDATE users SET `+strings.Join(sets, ",")+` WHERE id=?`, args...)
	return err
}

// UpdateUserPassword 更新密码散列。
func (s *MySqlStore) UpdateUserPassword(ctx context.Context, userID int64, passwordHash string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE users SET password_hash=? WHERE id=?`, passwordHash, userID)
	return err
}

// ==================== 公司 ====================

// CreateCompany 创建公司，并把 owner 加入为 owner 成员。
func (s *MySqlStore) CreateCompany(ctx context.Context, in CreateCompanyInput) (*types.Company, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `INSERT INTO companies (name, code, owner_id, created_at) VALUES (?,?,?,NOW(3))`, in.Name, in.Code, in.OwnerID)
	if err != nil {
		if isUniqueErr(err) {
			return nil, &AlreadyExistsError{Msg: "code taken"}
		}
		return nil, err
	}
	id, _ := res.LastInsertId()
	if _, err := tx.ExecContext(ctx, `INSERT INTO company_members (company_id, user_id, role, joined_at) VALUES (?,?, 'owner', NOW(3))`, id, in.OwnerID); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.GetCompanyByID(ctx, id)
}

// GetCompanyByID 按主键取公司。
func (s *MySqlStore) GetCompanyByID(ctx context.Context, id int64) (*types.Company, error) {
	var r types.CompanyRow
	err := s.db.QueryRowContext(ctx, `SELECT id, name, code, owner_id, created_at FROM companies WHERE id=?`, id).
		Scan(&r.ID, &r.Name, &r.Code, &r.OwnerID, &r.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
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
	err := s.db.QueryRowContext(ctx, `SELECT id, name, code, owner_id, created_at FROM companies WHERE code=?`, code).
		Scan(&r.ID, &r.Name, &r.Code, &r.OwnerID, &r.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
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
	err := s.db.QueryRowContext(ctx, `SELECT id, name, code, owner_id, created_at FROM companies WHERE name=?`, name).
		Scan(&r.ID, &r.Name, &r.Code, &r.OwnerID, &r.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
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
	q := `SELECT id, name, code, owner_id, created_at FROM companies`
	args := []any{}
	if kw := strings.TrimSpace(keyword); kw != "" {
		q += ` WHERE name LIKE ?`
		args = append(args, "%"+kw+"%")
	}
	q += ` ORDER BY id ASC LIMIT 100`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []types.Company{}
	for rows.Next() {
		var r types.CompanyRow
		if err := rows.Scan(&r.ID, &r.Name, &r.Code, &r.OwnerID, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, rowToCompany(&r))
	}
	return out, rows.Err()
}

// AddMember 加入公司（重复 → AlreadyExists）。
func (s *MySqlStore) AddMember(ctx context.Context, companyID, userID int64, role string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO company_members (company_id, user_id, role, joined_at) VALUES (?,?,?,NOW(3))`, companyID, userID, role)
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
	res, err := s.db.ExecContext(ctx, `DELETE FROM company_members WHERE company_id=? AND user_id=?`, companyID, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return &NotFoundError{Msg: "not a member"}
	}
	return nil
}

// SetRole 设置成员角色（不存在 → NotFound）。
func (s *MySqlStore) SetRole(ctx context.Context, companyID, userID int64, role string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE company_members SET role=? WHERE company_id=? AND user_id=?`, role, companyID, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return &NotFoundError{Msg: "not a member"}
	}
	return nil
}

// GetMemberRole 取成员角色（非成员返回空串+nil）。
func (s *MySqlStore) GetMemberRole(ctx context.Context, companyID, userID int64) (string, error) {
	var role string
	err := s.db.QueryRowContext(ctx, `SELECT role FROM company_members WHERE company_id=? AND user_id=?`, companyID, userID).Scan(&role)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return role, err
}

// GetMembers 列公司成员（联用户，joined_at 升序）。
func (s *MySqlStore) GetMembers(ctx context.Context, companyID int64) ([]types.Member, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT m.company_id, m.user_id, m.role, m.joined_at, u.username, u.nick FROM company_members m LEFT JOIN users u ON u.id=m.user_id WHERE m.company_id=? ORDER BY m.joined_at ASC`, companyID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []types.Member{}
	for rows.Next() {
		var m types.Member
		var username, nick sql.NullString
		if err := rows.Scan(&m.CompanyID, &m.UserID, &m.Role, &m.JoinedAt, &username, &nick); err != nil {
			return nil, err
		}
		if username.Valid {
			u := username.String
			m.Username = &u
		}
		if nick.Valid {
			n := nick.String
			m.Nick = &n
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// GetUserCompanies 取用户所属公司（联公司）。
func (s *MySqlStore) GetUserCompanies(ctx context.Context, userID int64) ([]types.Membership, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT c.id, c.name, c.code, c.owner_id, c.created_at, m.role FROM company_members m JOIN companies c ON c.id=m.company_id WHERE m.user_id=? ORDER BY m.company_id ASC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []types.Membership{}
	for rows.Next() {
		var cr types.CompanyRow
		var role string
		if err := rows.Scan(&cr.ID, &cr.Name, &cr.Code, &cr.OwnerID, &cr.CreatedAt, &role); err != nil {
			return nil, err
		}
		c := rowToCompany(&cr)
		out = append(out, types.Membership{Company: c, Role: role})
	}
	return out, rows.Err()
}

// RenameCompany 重命名公司。
func (s *MySqlStore) RenameCompany(ctx context.Context, companyID int64, name string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE companies SET name=? WHERE id=?`, name, companyID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return &NotFoundError{Msg: "company not found"}
	}
	return nil
}

// SetCompanyOwner 转让公司（更新 owner_id）。
func (s *MySqlStore) SetCompanyOwner(ctx context.Context, companyID, ownerID int64) error {
	res, err := s.db.ExecContext(ctx, `UPDATE companies SET owner_id=? WHERE id=?`, ownerID, companyID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
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
	if _, err := s.db.ExecContext(ctx, `DELETE dm FROM department_members dm JOIN departments d ON d.id=dm.department_id WHERE d.company_id=?`, companyID); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM departments WHERE company_id=?`, companyID); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM company_members WHERE company_id=?`, companyID); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM company_invitations WHERE company_id=?`, companyID); err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `DELETE FROM companies WHERE id=?`, companyID)
	return err
}

// listGroupsRows 取某公司全部群 id（供 DeleteCompany 内部用）。
func (s *MySqlStore) listGroupsRows(ctx context.Context, companyID int64) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM groups WHERE company_id=?`, companyID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ==================== 部门 ====================

// CreateDepartment 创建部门。
func (s *MySqlStore) CreateDepartment(ctx context.Context, in CreateDepartmentInput) (*types.Department, error) {
	res, err := s.db.ExecContext(ctx, `INSERT INTO departments (company_id, name, parent_id, created_at) VALUES (?,?,?,NOW(3))`,
		in.CompanyID, in.Name, in.ParentID)
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	return s.GetDepartmentByID(ctx, id)
}

// GetDepartmentByID 按主键取部门。
func (s *MySqlStore) GetDepartmentByID(ctx context.Context, id int64) (*types.Department, error) {
	var r types.DepartmentRow
	err := s.db.QueryRowContext(ctx, `SELECT id, company_id, name, parent_id, created_at FROM departments WHERE id=?`, id).
		Scan(&r.ID, &r.CompanyID, &r.Name, &r.ParentID, &r.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
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
	rows, err := s.db.QueryContext(ctx, `SELECT id, company_id, name, parent_id, created_at FROM departments WHERE company_id=? ORDER BY id ASC`, companyID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []types.Department{}
	for rows.Next() {
		var r types.DepartmentRow
		if err := rows.Scan(&r.ID, &r.CompanyID, &r.Name, &r.ParentID, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, rowToDepartment(&r))
	}
	return out, rows.Err()
}

// AssignDepartment 把用户加入部门。
func (s *MySqlStore) AssignDepartment(ctx context.Context, departmentID, userID int64) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO department_members (department_id, user_id, joined_at) VALUES (?,?,NOW(3))`, departmentID, userID)
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
	res, err := s.db.ExecContext(ctx, `DELETE FROM department_members WHERE department_id=? AND user_id=?`, departmentID, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return &NotFoundError{Msg: "not in department"}
	}
	return nil
}

// GetDepartmentMembers 列部门成员（联用户）。
func (s *MySqlStore) GetDepartmentMembers(ctx context.Context, departmentID int64) ([]types.DepartmentMember, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT m.department_id, m.user_id, m.joined_at, u.username, u.nick FROM department_members m LEFT JOIN users u ON u.id=m.user_id WHERE m.department_id=? ORDER BY m.joined_at ASC`, departmentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []types.DepartmentMember{}
	for rows.Next() {
		var m types.DepartmentMember
		var username, nick sql.NullString
		if err := rows.Scan(&m.DepartmentID, &m.UserID, &m.JoinedAt, &username, &nick); err != nil {
			return nil, err
		}
		if username.Valid {
			u := username.String
			m.Username = &u
		}
		if nick.Valid {
			n := nick.String
			m.Nick = &n
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// DeleteDepartment 删除部门并级联移除成员。
func (s *MySqlStore) DeleteDepartment(ctx context.Context, departmentID int64) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM department_members WHERE department_id=?`, departmentID); err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM departments WHERE id=?`, departmentID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return &NotFoundError{Msg: "department not found"}
	}
	return nil
}

// ==================== 群 ====================

// CreateGroup 创建群（companyId<=0 存 NULL），owner 自动入群。
func (s *MySqlStore) CreateGroup(ctx context.Context, in CreateGroupInput) (*types.Group, error) {
	var companyID any
	if in.CompanyID > 0 {
		companyID = in.CompanyID
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `INSERT INTO groups (company_id, department_id, name, code, owner_id, created_at) VALUES (?,?,?,?,?,NOW(3))`,
		companyID, in.DepartmentID, in.Name, in.Code, in.OwnerID)
	if err != nil {
		if isUniqueErr(err) {
			return nil, &AlreadyExistsError{Msg: "code taken"}
		}
		return nil, err
	}
	id, _ := res.LastInsertId()
	if _, err := tx.ExecContext(ctx, `INSERT INTO group_members (group_id, user_id, role, joined_at) VALUES (?,?,'owner',NOW(3))`, id, in.OwnerID); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.GetGroupByID(ctx, id)
}

// GetGroupByID 按主键取群。
func (s *MySqlStore) GetGroupByID(ctx context.Context, id int64) (*types.Group, error) {
	var r types.GroupRow
	err := s.db.QueryRowContext(ctx, `SELECT id, company_id, department_id, name, code, owner_id, created_at FROM groups WHERE id=?`, id).
		Scan(&r.ID, &r.CompanyID, &r.DepartmentID, &r.Name, &r.Code, &r.OwnerID, &r.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
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
		err = s.db.QueryRowContext(ctx, `SELECT id, company_id, department_id, name, code, owner_id, created_at FROM groups WHERE name=? AND company_id=?`, name, companyID).
			Scan(&r.ID, &r.CompanyID, &r.DepartmentID, &r.Name, &r.Code, &r.OwnerID, &r.CreatedAt)
	} else {
		err = s.db.QueryRowContext(ctx, `SELECT id, company_id, department_id, name, code, owner_id, created_at FROM groups WHERE name=? AND company_id IS NULL`, name).
			Scan(&r.ID, &r.CompanyID, &r.DepartmentID, &r.Name, &r.Code, &r.OwnerID, &r.CreatedAt)
	}
	if errors.Is(err, sql.ErrNoRows) {
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
	err := s.db.QueryRowContext(ctx, `SELECT id, company_id, department_id, name, code, owner_id, created_at FROM groups WHERE code=?`, code).
		Scan(&r.ID, &r.CompanyID, &r.DepartmentID, &r.Name, &r.Code, &r.OwnerID, &r.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
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
	q := `SELECT id, company_id, department_id, name, code, owner_id, created_at FROM groups WHERE 1=1`
	args := []any{}
	if companyID != 0 {
		q += ` AND company_id=?`
		args = append(args, companyID)
	}
	if kw := strings.TrimSpace(keyword); kw != "" {
		q += ` AND name LIKE ?`
		args = append(args, "%"+kw+"%")
	}
	q += ` ORDER BY id ASC LIMIT 100`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []types.Group{}
	for rows.Next() {
		var r types.GroupRow
		if err := rows.Scan(&r.ID, &r.CompanyID, &r.DepartmentID, &r.Name, &r.Code, &r.OwnerID, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, rowToGroup(&r))
	}
	return out, rows.Err()
}

// AddGroupMember 入群。
func (s *MySqlStore) AddGroupMember(ctx context.Context, groupID, userID int64, role string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO group_members (group_id, user_id, role, joined_at) VALUES (?,?,?,NOW(3))`, groupID, userID, role)
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
	res, err := s.db.ExecContext(ctx, `DELETE FROM group_members WHERE group_id=? AND user_id=?`, groupID, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return &NotFoundError{Msg: "not in group"}
	}
	return nil
}

// GetGroupMemberRole 取群成员角色。
func (s *MySqlStore) GetGroupMemberRole(ctx context.Context, groupID, userID int64) (string, error) {
	var role string
	err := s.db.QueryRowContext(ctx, `SELECT role FROM group_members WHERE group_id=? AND user_id=?`, groupID, userID).Scan(&role)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return role, err
}

// GetGroupMember 取群成员（含禁言截止，ISO 字符串）。
func (s *MySqlStore) GetGroupMember(ctx context.Context, groupID, userID int64) (string, *string, error) {
	var role string
	var muted sql.NullTime
	err := s.db.QueryRowContext(ctx, `SELECT role, muted_until FROM group_members WHERE group_id=? AND user_id=?`, groupID, userID).Scan(&role, &muted)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil, nil
	}
	if err != nil {
		return "", nil, err
	}
	var mutedPtr *string
	if muted.Valid && !muted.Time.IsZero() {
		s := toIso(muted.Time)
		mutedPtr = &s
	}
	return role, mutedPtr, nil
}

// SetGroupMemberRole 设置群成员角色。
func (s *MySqlStore) SetGroupMemberRole(ctx context.Context, groupID, userID int64, role string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE group_members SET role=? WHERE group_id=? AND user_id=?`, role, groupID, userID)
	return err
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
	_, err := s.db.ExecContext(ctx, `UPDATE group_members SET muted_until=? WHERE group_id=? AND user_id=?`, val, groupID, userID)
	return err
}

// SetGroupOwner 转让群主。
func (s *MySqlStore) SetGroupOwner(ctx context.Context, groupID, ownerID int64) error {
	res, err := s.db.ExecContext(ctx, `UPDATE groups SET owner_id=? WHERE id=?`, ownerID, groupID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return &NotFoundError{Msg: "group not found"}
	}
	return nil
}

// DeleteGroup 解散群：删群会话消息/成员，删群成员。
func (s *MySqlStore) DeleteGroup(ctx context.Context, groupID int64) error {
	convs, err := s.db.QueryContext(ctx, `SELECT id FROM conversations WHERE type='group' AND group_id=?`, groupID)
	if err != nil {
		return err
	}
	var convIDs []int64
	for convs.Next() {
		var id int64
		if err := convs.Scan(&id); err != nil {
			convs.Close()
			return err
		}
		convIDs = append(convIDs, id)
	}
	convs.Close()
	for _, cid := range convIDs {
		if _, err := s.db.ExecContext(ctx, `DELETE FROM messages WHERE conversation_id=?`, cid); err != nil {
			return err
		}
		if _, err := s.db.ExecContext(ctx, `DELETE FROM conversation_members WHERE conversation_id=?`, cid); err != nil {
			return err
		}
		if _, err := s.db.ExecContext(ctx, `DELETE FROM conversations WHERE id=?`, cid); err != nil {
			return err
		}
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM group_members WHERE group_id=?`, groupID); err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `DELETE FROM groups WHERE id=?`, groupID)
	return err
}

// GetGroupMembers 列群成员（联用户）。
func (s *MySqlStore) GetGroupMembers(ctx context.Context, groupID int64) ([]types.GroupMember, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT m.group_id, m.user_id, m.role, m.joined_at, m.muted_until, u.username, u.nick FROM group_members m LEFT JOIN users u ON u.id=m.user_id WHERE m.group_id=? ORDER BY m.joined_at ASC`, groupID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []types.GroupMember{}
	for rows.Next() {
		var m types.GroupMember
		var username, nick sql.NullString
		var muted sql.NullTime
		if err := rows.Scan(&m.GroupID, &m.UserID, &m.Role, &m.JoinedAt, &muted, &username, &nick); err != nil {
			return nil, err
		}
		if username.Valid {
			u := username.String
			m.Username = &u
		}
		if nick.Valid {
			n := nick.String
			m.Nick = &n
		}
		if muted.Valid && !muted.Time.IsZero() {
			s := toIso(muted.Time)
			m.MutedUntil = &s
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// GetUserGroups 取用户所在群。
func (s *MySqlStore) GetUserGroups(ctx context.Context, userID int64) ([]types.Group, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT g.id, g.company_id, g.department_id, g.name, g.code, g.owner_id, g.created_at FROM group_members m JOIN groups g ON g.id=m.group_id WHERE m.user_id=? ORDER BY m.group_id ASC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []types.Group{}
	for rows.Next() {
		var r types.GroupRow
		if err := rows.Scan(&r.ID, &r.CompanyID, &r.DepartmentID, &r.Name, &r.Code, &r.OwnerID, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, rowToGroup(&r))
	}
	return out, rows.Err()
}
