// Package job 实现公司职位系统的 socket 事件。
//
// 事件（requireMember 公司隔离；create/update/delete/bind = admin/owner，list/bindings = 成员）：
//   - job:create / update / delete / list
//   - job:bind（绑定/解绑成员职位）/ job:bindings（公司全部成员绑定视图）
//
// 默认职位清单在首次 list/create 时懒播种（db 层 ensure，不重复）。
package job

import (
	"context"
	"encoding/json"
	"math"
	"strconv"
	"strings"

	"mochi-oa-server-go/internal/db"
	"mochi-oa-server-go/internal/logger"
	"mochi-oa-server-go/internal/socketapi"
	"mochi-oa-server-go/internal/types"
	"mochi-oa-server-go/internal/util"
)

const mod = "handlers/job"

// RegisterJobHandlers 注册职位全部 socket 事件。
func RegisterJobHandlers(srv socketapi.Server, store db.Store) {
	ctx := context.Background()

	// ---- job:create（admin/owner） ----
	srv.On("job:create", func(c socketapi.Conn, data json.RawMessage, ack socketapi.Ack) {
		d := parseData(data)
		_, cid, ok := requireAdmin(c, d, store, ack)
		if !ok {
			return
		}
		name := strings.TrimSpace(strField(d, "name"))
		if name == "" {
			ack(util.Fail("职位名称不能为空"))
			return
		}
		job, err := store.CreateJob(ctx, db.CreateJobInput{
			CompanyID:   cid,
			Name:        name,
			Description: strPtr(d, "description"),
			SortOrder:   toInt64Default(d["sortOrder"], 0),
		})
		if err != nil {
			logger.Error(mod, "job:create 失败", "companyId", cid, "error", err.Error())
			ack(util.Fail("创建失败"))
			return
		}
		ack(util.OK(map[string]any{"job": job}))
	})

	// ---- job:update（admin/owner，仅本公司职位） ----
	srv.On("job:update", func(c socketapi.Conn, data json.RawMessage, ack socketapi.Ack) {
		d := parseData(data)
		_, cid, ok := requireAdmin(c, d, store, ack)
		if !ok {
			return
		}
		id, ok := toInt64(d["id"])
		if !ok || id <= 0 {
			ack(util.Fail("参数不合法"))
			return
		}
		in := db.UpdateJobInput{}
		if v, has := d["name"]; has && v != nil {
			s := strings.TrimSpace(toString(v))
			in.Name = &s
		}
		if v, has := d["description"]; has && v != nil {
			s := toString(v)
			in.Description = &s
		}
		if v, has := d["enabled"]; has && v != nil {
			b, ok := v.(bool)
			if !ok {
				ack(util.Fail("enabled 不合法"))
				return
			}
			in.Enabled = &b
		}
		if v, has := d["sortOrder"]; has && v != nil {
			n := toInt64Default(v, 0)
			in.SortOrder = &n
		}
		job, err := store.UpdateJob(ctx, id, in)
		if err != nil {
			logger.Error(mod, "job:update 失败", "id", id, "error", err.Error())
			ack(util.Fail("更新失败"))
			return
		}
		if job == nil || job.CompanyID != cid {
			ack(util.Fail("职位不存在"))
			return
		}
		ack(util.OK(map[string]any{"job": job}))
	})

	// ---- job:delete（admin/owner；绑定成员自动解绑） ----
	srv.On("job:delete", func(c socketapi.Conn, data json.RawMessage, ack socketapi.Ack) {
		d := parseData(data)
		_, _, ok := requireAdmin(c, d, store, ack)
		if !ok {
			return
		}
		id, ok := toInt64(d["id"])
		if !ok || id <= 0 {
			ack(util.Fail("参数不合法"))
			return
		}
		if err := store.DeleteJob(ctx, id); err != nil {
			logger.Error(mod, "job:delete 失败", "id", id, "error", err.Error())
			ack(util.Fail("删除失败"))
			return
		}
		ack(util.OK(map[string]any{}))
	})

	// ---- job:list（成员；懒播种默认清单） ----
	srv.On("job:list", func(c socketapi.Conn, data json.RawMessage, ack socketapi.Ack) {
		d := parseData(data)
		_, cid, _, ok := requireMember(c, d, store, ack)
		if !ok {
			return
		}
		keyword := strings.TrimSpace(strField(d, "keyword"))
		enabledOnly := false
		if v, has := d["enabledOnly"]; has {
			enabledOnly, _ = v.(bool)
		}
		page, pageSize := pageArgs(d, 100)
		jobs, total, err := store.ListJobs(ctx, cid, keyword, enabledOnly, page, pageSize)
		if err != nil {
			logger.Error(mod, "job:list 失败", "companyId", cid, "error", err.Error())
			ack(util.Fail("获取失败"))
			return
		}
		if jobs == nil {
			jobs = []types.JobItem{}
		}
		ack(util.OK(map[string]any{"jobs": jobs, "total": total}))
	})

	// ---- job:bind（admin；jobId 可空=解绑；extra=兼任标记） ----
	srv.On("job:bind", func(c socketapi.Conn, data json.RawMessage, ack socketapi.Ack) {
		d := parseData(data)
		_, cid, ok := requireAdmin(c, d, store, ack)
		if !ok {
			return
		}
		userID, ok := toInt64(d["userId"])
		if !ok || userID <= 0 {
			ack(util.Fail("参数不合法"))
			return
		}
		// userId 必须本公司成员。
		if role, err := store.GetMemberRole(ctx, cid, userID); err != nil || role == "" {
			logger.Error(mod, "job:bind 员工不在该公司", "companyId", cid, "userId", userID)
			ack(util.Fail("员工不在该公司"))
			return
		}
		var jobID *int64
		if v, present := d["jobId"]; present && v != nil {
			jid, ok := toInt64(v)
			if !ok || jid <= 0 {
				ack(util.Fail("职位不合法"))
				return
			}
			// jobId 必须本公司职位。
			if !jobBelongsCompany(ctx, store, cid, jid) {
				logger.Error(mod, "job:bind 职位不在该公司", "companyId", cid, "jobId", jid)
				ack(util.Fail("职位不在该公司"))
				return
			}
			jobID = &jid
		}
		var extra *string
		if v, present := d["extra"]; present && v != nil {
			s := toString(v)
			extra = &s
		}
		if err := store.BindPosition(ctx, cid, userID, jobID, extra); err != nil {
			logger.Error(mod, "job:bind 失败", "companyId", cid, "userId", userID, "error", err.Error())
			ack(util.Fail("绑定失败"))
			return
		}
		ack(util.OK(map[string]any{"binding": map[string]any{
			"userId": userID, "positionId": jobID, "positionExtra": extra,
		}}))
	})

	// ---- job:bindings（成员；公司全部成员绑定视图） ----
	srv.On("job:bindings", func(c socketapi.Conn, data json.RawMessage, ack socketapi.Ack) {
		d := parseData(data)
		_, cid, _, ok := requireMember(c, d, store, ack)
		if !ok {
			return
		}
		page, pageSize := pageArgs(d, 50)
		bindings, total, err := store.ListBindings(ctx, cid, page, pageSize)
		if err != nil {
			logger.Error(mod, "job:bindings 失败", "companyId", cid, "error", err.Error())
			ack(util.Fail("获取失败"))
			return
		}
		if bindings == nil {
			bindings = []types.BindingItem{}
		}
		ack(util.OK(map[string]any{"bindings": bindings, "total": total}))
	})
}

// jobBelongsCompany 校验职位属于该公司（通过 ListJobs 全量匹配；职位量小可接受）。
func jobBelongsCompany(ctx context.Context, store db.Store, cid, jobID int64) bool {
	jobs, _, err := store.ListJobs(ctx, cid, "", false, 1, 100)
	if err != nil {
		return false
	}
	for _, j := range jobs {
		if j.ID == jobID {
			return true
		}
	}
	return false
}

// ─── 私有工具（同构副本） ───

func getAuth(c socketapi.Conn) *types.Auth {
	v := c.Get("auth")
	if a, ok := v.(*types.Auth); ok && a != nil {
		return a
	}
	return nil
}

func parseData(data json.RawMessage) map[string]any {
	m := map[string]any{}
	if len(data) == 0 {
		return m
	}
	_ = json.Unmarshal(data, &m)
	return m
}

func toInt64(v any) (int64, bool) {
	switch n := v.(type) {
	case float64:
		if n != math.Trunc(n) {
			return 0, false
		}
		return int64(n), true
	case string:
		i, err := strconv.ParseInt(strings.TrimSpace(n), 10, 64)
		if err != nil {
			return 0, false
		}
		return i, true
	}
	return 0, false
}

func toInt64Default(v any, dft int64) int64 {
	if n, ok := toInt64(v); ok {
		return n
	}
	return dft
}

func strField(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

func toString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	}
	return ""
}

func strPtr(m map[string]any, key string) *string {
	if v, present := m[key]; present && v != nil {
		s := toString(v)
		return &s
	}
	return nil
}

func roleRank(role string) int {
	switch role {
	case "owner":
		return 3
	case "admin":
		return 2
	default:
		return 1
	}
}

func requireMember(c socketapi.Conn, d map[string]any, st db.Store, ack socketapi.Ack) (*types.Auth, int64, string, bool) {
	a := getAuth(c)
	if a == nil {
		ack(util.Fail("未登录"))
		return nil, 0, "", false
	}
	cid, ok := toInt64(d["companyId"])
	if !ok || cid <= 0 {
		ack(util.Fail("参数不合法"))
		return nil, 0, "", false
	}
	role, err := st.GetMemberRole(context.Background(), cid, a.ID)
	if err != nil {
		logger.Error(mod, "查询成员角色失败", "companyId", cid, "userId", a.ID, "error", err.Error())
		ack(util.Fail("操作失败"))
		return nil, 0, "", false
	}
	if role == "" {
		ack(util.Fail("未加入该公司"))
		return nil, 0, "", false
	}
	return a, cid, role, true
}

func requireAdmin(c socketapi.Conn, d map[string]any, st db.Store, ack socketapi.Ack) (*types.Auth, int64, bool) {
	a, cid, role, ok := requireMember(c, d, st, ack)
	if !ok {
		return nil, 0, false
	}
	if roleRank(role) < roleRank("admin") {
		ack(util.Fail("仅管理员可操作"))
		return nil, 0, false
	}
	return a, cid, true
}

func pageArgs(d map[string]any, dftSize int64) (int64, int64) {
	page := int64(1)
	if v, present := d["page"]; present && v != nil {
		if n, ok := toInt64(v); ok && n > 0 {
			page = n
		}
	}
	size := dftSize
	if v, present := d["pageSize"]; present && v != nil {
		if n, ok := toInt64(v); ok && n > 0 {
			size = n
		}
	}
	if size > 100 {
		size = 100
	}
	return page, size
}
