// Package db 的 MemoryStore：进程内 map 实现，STORAGE=memory 时使用（开发/测试）。
//
// 行为对齐 memory-store.ts：用 map 存全部实体，自增 id；
// 唯一约束/重复加入返回对应错误。不追求并发极致，单实例开发足够。
package db

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"mochi-oa-server-go/internal/types"
)

// MemoryStore 是内存存储。
type MemoryStore struct {
	seq int64

	users      map[int64]*types.UserRow
	companies  map[int64]*types.CompanyRow
	companyMem map[int64]map[int64]*types.CompanyMemberRow

	departments map[int64]*types.DepartmentRow
	deptMem     map[int64]map[int64]*types.DepartmentMemberRow

	groups    map[int64]*types.GroupRow
	groupMem  map[int64]map[int64]*types.GroupMemberRow
	groupConv map[int64]int64 // groupID → conversationID

	conversations       map[int64]*types.ConversationRow
	convMem             map[int64]map[int64]*types.ConversationMemberRow
	dmConv              map[string]int64 // "a:b" (x<y) → conversationID

	messages map[int64]*types.MessageRow

	files map[string]*types.FileRow

	friends map[string]*types.FriendRow // "a:b"

	invitations map[int64]*types.CompanyInvitationRow

	profiles map[string]*types.MemberProfileRow // "companyID:userID"

	sessions map[int64]*types.SessionRow

	mailCfgs map[int64]*types.MailConfigRow

	holidays map[string]*types.HolidayRow // date → row

	projects     map[int64]*types.ProjectRow
	projectMem   map[int64]map[int64]*types.ProjectMemberRow
	tasks        map[int64]*types.TaskRow
	assignments  map[int64]*types.TaskAssignmentRow
	taskComments map[int64]*types.TaskCommentRow
	taskIssues   map[int64]*types.TaskIssueRow
	taskLogs     map[int64]*types.TaskStatusLogRow
	taskExts     map[int64]*types.TaskExtensionRow

	requirements map[int64]*types.RequirementRow
	bugs         map[int64]*types.BugRow
	plans        map[int64]*types.PlanRow
	documents    map[int64]*types.DocumentRow
	wikiPages    map[int64]*types.WikiPageRow
	reqLinks     map[int64]*types.RequirementLinkRow

	afdianOrders map[int64]*types.AfdianOrderRow
}

// NewMemoryStore 构造空内存库。
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		users: map[int64]*types.UserRow{}, companies: map[int64]*types.CompanyRow{},
		companyMem: map[int64]map[int64]*types.CompanyMemberRow{}, departments: map[int64]*types.DepartmentRow{},
		deptMem: map[int64]map[int64]*types.DepartmentMemberRow{}, groups: map[int64]*types.GroupRow{},
		groupMem: map[int64]map[int64]*types.GroupMemberRow{}, groupConv: map[int64]int64{},
		conversations: map[int64]*types.ConversationRow{}, convMem: map[int64]map[int64]*types.ConversationMemberRow{},
		dmConv: map[string]int64{}, messages: map[int64]*types.MessageRow{}, files: map[string]*types.FileRow{},
		friends: map[string]*types.FriendRow{}, invitations: map[int64]*types.CompanyInvitationRow{},
		profiles: map[string]*types.MemberProfileRow{}, sessions: map[int64]*types.SessionRow{},
		mailCfgs: map[int64]*types.MailConfigRow{}, holidays: map[string]*types.HolidayRow{},
		projects: map[int64]*types.ProjectRow{}, projectMem: map[int64]map[int64]*types.ProjectMemberRow{},
		tasks: map[int64]*types.TaskRow{}, assignments: map[int64]*types.TaskAssignmentRow{},
		taskComments: map[int64]*types.TaskCommentRow{}, taskIssues: map[int64]*types.TaskIssueRow{},
		taskLogs: map[int64]*types.TaskStatusLogRow{}, taskExts: map[int64]*types.TaskExtensionRow{},
		requirements: map[int64]*types.RequirementRow{}, bugs: map[int64]*types.BugRow{},
		plans: map[int64]*types.PlanRow{}, documents: map[int64]*types.DocumentRow{},
		wikiPages: map[int64]*types.WikiPageRow{}, reqLinks: map[int64]*types.RequirementLinkRow{},
		afdianOrders: map[int64]*types.AfdianOrderRow{},
	}
}

// nextId 自增主键。
func (m *MemoryStore) nextId() int64 {
	m.seq++
	return m.seq
}

func normPair(a, b int64) (int64, int64) {
	if a <= b {
		return a, b
	}
	return b, a
}

func pairKey(a, b int64) string { return fmt.Sprintf("%d:%d", a, b) }

// Init 空实现（内存库无需初始化）。
func (m *MemoryStore) Init(_ context.Context) error { return nil }

// Close 空实现。
func (m *MemoryStore) Close(_ context.Context) error { return nil }

// ==================== 用户 ====================

func (m *MemoryStore) CreateUser(_ context.Context, in CreateUserInput) (*types.User, error) {
	for _, u := range m.users {
		if u.Username == in.Username {
			return nil, &AlreadyExistsError{Msg: "username taken"}
		}
	}
	id := m.nextId()
	m.users[id] = &types.UserRow{ID: id, Username: in.Username, PasswordHash: in.PasswordHash, Nick: in.Nick, Avatar: in.Avatar, Email: in.Email, Phone: in.Phone, CreatedAt: time.Now().UTC()}
	u := rowToUser(m.users[id])
	return &u, nil
}

func (m *MemoryStore) findUserByName(username string) *types.UserRow {
	for _, u := range m.users {
		if u.Username == username {
			return u
		}
	}
	return nil
}

func (m *MemoryStore) GetUserByUsername(_ context.Context, username string) (*types.UserWithPassword, error) {
	if r := m.findUserByName(username); r != nil {
		return &types.UserWithPassword{User: rowToUser(r), PasswordHash: r.PasswordHash}, nil
	}
	return nil, nil
}

func (m *MemoryStore) GetUserByEmail(_ context.Context, email string) (*types.UserWithPassword, error) {
	for _, u := range m.users {
		if strings.EqualFold(u.Email, email) && u.Email != "" {
			return &types.UserWithPassword{User: rowToUser(u), PasswordHash: u.PasswordHash}, nil
		}
	}
	return nil, nil
}

func (m *MemoryStore) GetUserByID(_ context.Context, id int64) (*types.User, error) {
	if r, ok := m.users[id]; ok {
		u := rowToUser(r)
		return &u, nil
	}
	return nil, nil
}

func (m *MemoryStore) ListUsers(_ context.Context) ([]types.User, error) {
	ids := []int64{}
	for id := range m.users {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	out := []types.User{}
	for _, id := range ids {
		out = append(out, rowToUser(m.users[id]))
	}
	return out, nil
}

func (m *MemoryStore) UpdateUserProfile(_ context.Context, userID int64, patch UserProfilePatch) error {
	u := m.users[userID]
	if u == nil {
		return nil
	}
	if patch.Nick != nil {
		u.Nick = *patch.Nick
	}
	if patch.Avatar != nil {
		u.Avatar = *patch.Avatar
	}
	if patch.Phone != nil {
		u.Phone = *patch.Phone
	}
	if patch.Email != nil {
		u.Email = *patch.Email
	}
	if patch.Extra != nil {
		u.Extra = patch.Extra
	}
	return nil
}

func (m *MemoryStore) UpdateUserPassword(_ context.Context, userID int64, passwordHash string) error {
	if u := m.users[userID]; u != nil {
		u.PasswordHash = passwordHash
	}
	return nil
}

// ==================== 公司 ====================

func (m *MemoryStore) CreateCompany(_ context.Context, in CreateCompanyInput) (*types.Company, error) {
	for _, c := range m.companies {
		if c.Code == in.Code {
			return nil, &AlreadyExistsError{Msg: "code taken"}
		}
	}
	id := m.nextId()
	m.companies[id] = &types.CompanyRow{ID: id, Name: in.Name, Code: in.Code, OwnerID: in.OwnerID, CreatedAt: time.Now().UTC()}
	mm := map[int64]*types.CompanyMemberRow{in.OwnerID: {CompanyID: id, UserID: in.OwnerID, Role: "owner", JoinedAt: time.Now().UTC()}}
	m.companyMem[id] = mm
	c := rowToCompany(m.companies[id])
	return &c, nil
}

func (m *MemoryStore) GetCompanyByID(_ context.Context, id int64) (*types.Company, error) {
	if r, ok := m.companies[id]; ok {
		c := rowToCompany(r)
		return &c, nil
	}
	return nil, nil
}

func (m *MemoryStore) GetCompanyByCode(_ context.Context, code string) (*types.Company, error) {
	for _, r := range m.companies {
		if r.Code == code {
			c := rowToCompany(r)
			return &c, nil
		}
	}
	return nil, nil
}

func (m *MemoryStore) GetCompanyByName(_ context.Context, name string) (*types.Company, error) {
	for _, r := range m.companies {
		if r.Name == name {
			c := rowToCompany(r)
			return &c, nil
		}
	}
	return nil, nil
}

func (m *MemoryStore) ListCompanies(_ context.Context, keyword string) ([]types.Company, error) {
	ids := []int64{}
	for id := range m.companies {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	out := []types.Company{}
	for _, id := range ids {
		c := rowToCompany(m.companies[id])
		if kw := strings.TrimSpace(keyword); kw != "" && !strings.Contains(c.Name, kw) {
			continue
		}
		out = append(out, c)
	}
	return out, nil
}

func (m *MemoryStore) AddMember(_ context.Context, companyID, userID int64, role string) error {
	set := m.companyMem[companyID]
	if set == nil {
		set = map[int64]*types.CompanyMemberRow{}
		m.companyMem[companyID] = set
	}
	if _, ok := set[userID]; ok {
		return &AlreadyExistsError{Msg: "already a member"}
	}
	set[userID] = &types.CompanyMemberRow{CompanyID: companyID, UserID: userID, Role: role, JoinedAt: time.Now().UTC()}
	return nil
}

func (m *MemoryStore) RemoveMember(_ context.Context, companyID, userID int64) error {
	if set := m.companyMem[companyID]; set != nil {
		if _, ok := set[userID]; ok {
			delete(set, userID)
			return nil
		}
	}
	return &NotFoundError{Msg: "not a member"}
}

func (m *MemoryStore) SetRole(_ context.Context, companyID, userID int64, role string) error {
	if set := m.companyMem[companyID]; set != nil {
		if r, ok := set[userID]; ok {
			r.Role = role
			return nil
		}
	}
	return &NotFoundError{Msg: "not a member"}
}

func (m *MemoryStore) GetMemberRole(_ context.Context, companyID, userID int64) (string, error) {
	if set := m.companyMem[companyID]; set != nil {
		if r, ok := set[userID]; ok {
			return r.Role, nil
		}
	}
	return "", nil
}

func (m *MemoryStore) GetMembers(_ context.Context, companyID int64) ([]types.Member, error) {
	out := []types.Member{}
	if set := m.companyMem[companyID]; set != nil {
		for _, r := range set {
			mem := types.Member{CompanyID: r.CompanyID, UserID: r.UserID, Role: r.Role, JoinedAt: toIso(r.JoinedAt)}
			if u, _ := m.GetUserByID(context.Background(), r.UserID); u != nil {
				un, nk := u.Username, u.Nick
				mem.Username, mem.Nick = &un, &nk
			}
			out = append(out, mem)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].JoinedAt < out[j].JoinedAt })
	return out, nil
}

func (m *MemoryStore) GetUserCompanies(_ context.Context, userID int64) ([]types.Membership, error) {
	out := []types.Membership{}
	for cid, set := range m.companyMem {
		if r, ok := set[userID]; ok {
			if c, err := m.GetCompanyByID(context.Background(), cid); err == nil && c != nil {
				out = append(out, types.Membership{Company: *c, Role: r.Role})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Company.ID < out[j].Company.ID })
	return out, nil
}

func (m *MemoryStore) RenameCompany(_ context.Context, companyID int64, name string) error {
	if c, ok := m.companies[companyID]; ok {
		c.Name = name
		return nil
	}
	return &NotFoundError{Msg: "company not found"}
}

func (m *MemoryStore) SetCompanyOwner(_ context.Context, companyID, ownerID int64) error {
	if c, ok := m.companies[companyID]; ok {
		c.OwnerID = ownerID
		return nil
	}
	return &NotFoundError{Msg: "company not found"}
}

func (m *MemoryStore) DeleteCompany(_ context.Context, companyID int64) error {
	for gid, g := range m.groups {
		if g.CompanyID != nil && *g.CompanyID == companyID {
			_ = m.DeleteGroup(context.Background(), gid)
		}
	}
	delete(m.deptMem, companyID)
	for did, d := range m.departments {
		if d.CompanyID == companyID {
			delete(m.departments, did)
		}
	}
	delete(m.companyMem, companyID)
	delete(m.invitations, companyID)
	delete(m.companies, companyID)
	return nil
}

// ==================== 部门 ====================

func (m *MemoryStore) CreateDepartment(_ context.Context, in CreateDepartmentInput) (*types.Department, error) {
	id := m.nextId()
	m.departments[id] = &types.DepartmentRow{ID: id, CompanyID: in.CompanyID, Name: in.Name, ParentID: in.ParentID, CreatedAt: time.Now().UTC()}
	d := rowToDepartment(m.departments[id])
	return &d, nil
}

func (m *MemoryStore) GetDepartmentByID(_ context.Context, id int64) (*types.Department, error) {
	if r, ok := m.departments[id]; ok {
		d := rowToDepartment(r)
		return &d, nil
	}
	return nil, nil
}

func (m *MemoryStore) ListDepartments(_ context.Context, companyID int64) ([]types.Department, error) {
	out := []types.Department{}
	for _, r := range m.departments {
		if r.CompanyID == companyID {
			out = append(out, rowToDepartment(r))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (m *MemoryStore) AssignDepartment(_ context.Context, departmentID, userID int64) error {
	set := m.deptMem[departmentID]
	if set == nil {
		set = map[int64]*types.DepartmentMemberRow{}
		m.deptMem[departmentID] = set
	}
	if _, ok := set[userID]; ok {
		return &AlreadyExistsError{Msg: "already in department"}
	}
	set[userID] = &types.DepartmentMemberRow{DepartmentID: departmentID, UserID: userID, JoinedAt: time.Now().UTC()}
	return nil
}

func (m *MemoryStore) RemoveDepartmentMember(_ context.Context, departmentID, userID int64) error {
	if set := m.deptMem[departmentID]; set != nil {
		if _, ok := set[userID]; ok {
			delete(set, userID)
			return nil
		}
	}
	return &NotFoundError{Msg: "not in department"}
}

func (m *MemoryStore) GetDepartmentMembers(_ context.Context, departmentID int64) ([]types.DepartmentMember, error) {
	out := []types.DepartmentMember{}
	if set := m.deptMem[departmentID]; set != nil {
		for _, r := range set {
			dm := types.DepartmentMember{DepartmentID: r.DepartmentID, UserID: r.UserID, JoinedAt: toIso(r.JoinedAt)}
			if u, _ := m.GetUserByID(context.Background(), r.UserID); u != nil {
				un, nk := u.Username, u.Nick
				dm.Username, dm.Nick = &un, &nk
			}
			out = append(out, dm)
		}
	}
	return out, nil
}

func (m *MemoryStore) DeleteDepartment(_ context.Context, departmentID int64) error {
	delete(m.deptMem, departmentID)
	delete(m.departments, departmentID)
	return nil
}
