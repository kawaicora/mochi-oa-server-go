// Package org —— org.go 1:1 复刻 server/src/handlers/org.ts（236 行）：
//   - org:importCsv：从 CSV 导入组织架构（公司 → 部门 → 成员）
//
// 列：company, company_code, department, username, password, nick, role
// 导入人须已登录；新建公司时导入人自动成为 owner(创建人)。幂等：已存在则跳过。
package org

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"

	"mochi-oa-server-go/internal/db"
	"mochi-oa-server-go/internal/logger"
	"mochi-oa-server-go/internal/password"
	"mochi-oa-server-go/internal/socketapi"
	"mochi-oa-server-go/internal/util"
)

// isPassword 校验密码长度：6-128 位。
func isPassword(v any) bool {
	s, ok := v.(string)
	if !ok {
		return false
	}
	return len(s) >= 6 && len(s) <= 128
}

// csvRow 是解析后的一行 CSV 数据。
type csvRow struct {
	company      string
	companyCode  string
	department   string
	username     string
	password     string
	nick         string
	role         string
}

// parseCsv 简易 CSV 解析：支持引号包裹、逗号内逗号、BOM、CRLF。
// 对标 org.ts 的 parseCsv 函数。
func parseCsv(text string) []csvRow {
	// 去 BOM、统一换行
	src := strings.TrimPrefix(text, "\uFEFF")
	src = strings.ReplaceAll(src, "\r\n", "\n")
	src = strings.ReplaceAll(src, "\r", "\n")

	var lines [][]string
	var row []string
	var field strings.Builder
	inQuote := false

	for i := 0; i < len(src); i++ {
		c := src[i]
		if inQuote {
			if c == '"' {
				if i+1 < len(src) && src[i+1] == '"' {
					field.WriteByte('"')
					i++
				} else {
					inQuote = false
				}
			} else {
				field.WriteByte(c)
			}
		} else if c == '"' {
			inQuote = true
		} else if c == ',' {
			row = append(row, field.String())
			field.Reset()
		} else if c == '\n' {
			row = append(row, field.String())
			lines = append(lines, row)
			row = nil
			field.Reset()
		} else {
			field.WriteByte(c)
		}
	}
	if field.Len() > 0 || len(row) > 0 {
		row = append(row, field.String())
		lines = append(lines, row)
	}

	// 去掉全空行
	var nonEmpty [][]string
	for _, r := range lines {
		hasContent := false
		for _, c := range r {
			if strings.TrimSpace(c) != "" {
				hasContent = true
				break
			}
		}
		if hasContent {
			nonEmpty = append(nonEmpty, r)
		}
	}
	if len(nonEmpty) == 0 {
		return nil
	}

	// 表头归一（兼容常见别名）
	head := make([]string, len(nonEmpty[0]))
	for i, h := range nonEmpty[0] {
		head[i] = strings.ToLower(strings.TrimSpace(h))
	}

	type aliasPair struct {
		key    string
		names  []string
	}
	aliases := []aliasPair{
		{"company", []string{"company", "company_name", "公司", "组织", "组织名"}},
		{"company_code", []string{"company_code", "companycode", "code", "加入码", "公司码"}},
		{"department", []string{"department", "dept", "部门", "部门名", "department_name"}},
		{"username", []string{"username", "user", "账号", "用户名", "登录名"}},
		{"password", []string{"password", "pwd", "密码"}},
		{"nick", []string{"nick", "nickname", "name", "昵称", "姓名"}},
		{"role", []string{"role", "职位", "角色", "权限"}},
	}

	colIndex := map[string]int{}
	for _, a := range aliases {
		for i, h := range head {
			for _, n := range a.names {
				if h == n {
					if _, exists := colIndex[a.key]; !exists {
						colIndex[a.key] = i
					}
					break
				}
			}
		}
	}

	// 判断是否有有效表头
	_, hasCompany := colIndex["company"]
	_, hasUsername := colIndex["username"]
	isHeader := hasCompany || hasUsername

	getCol := func(r []string, key string) string {
		if idx, ok := colIndex[key]; ok && idx < len(r) {
			return r[idx]
		}
		return ""
	}

	var rows []csvRow
	if isHeader {
		for _, r := range nonEmpty[1:] {
			rows = append(rows, csvRow{
				company:     getCol(r, "company"),
				companyCode: getCol(r, "company_code"),
				department:  getCol(r, "department"),
				username:    getCol(r, "username"),
				password:    getCol(r, "password"),
				nick:        getCol(r, "nick"),
				role:        getCol(r, "role"),
			})
		}
	} else {
		for _, r := range nonEmpty {
			safe := func(i int) string {
				if i < len(r) {
					return r[i]
				}
				return ""
			}
			rows = append(rows, csvRow{
				company:     safe(0),
				companyCode: safe(1),
				department:  safe(2),
				username:    safe(3),
				password:    safe(4),
				nick:        safe(5),
				role:        safe(6),
			})
		}
	}
	return rows
}

// RegisterOrgHandlers 注册组织导入相关 socket 事件。
// 对标 registerOrgHandlers(ctx)。
func RegisterOrgHandlers(srv socketapi.Server) {
	store := db.GetStore()
	ctx := context.Background()

	_ = srv.OnConnection // 本 handler 无 per-connection 状态

	srv.On("org:importCsv", func(c socketapi.Conn, data json.RawMessage, ack socketapi.Ack) {
		a := getAuth(c)
		if a == nil {
			ack(util.Fail("未登录"))
			return
		}
		d := parseData(data)
		csvText, _ := d["csv"].(string)
		if strings.TrimSpace(csvText) == "" {
			ack(util.Fail("CSV 内容为空"))
			return
		}
		if len(csvText) > 2_000_000 {
			ack(util.Fail("CSV 过大（限 2MB）"))
			return
		}

		rows := parseCsv(csvText)

		type importResult struct {
			Ok          bool     `json:"ok"`
			Companies   int      `json:"companies"`
			Departments int      `json:"departments"`
			Users       int      `json:"users"`
			Memberships int      `json:"memberships"`
			Skipped     int      `json:"skipped"`
			Errors      []string `json:"errors"`
		}
		result := importResult{Ok: true, Errors: []string{}}

		companyCache := map[string]int64{} // code → companyId
		deptCache := map[string]int64{}   // "companyId:deptName" → deptId

		for idx, r := range rows {
			companyName := strings.TrimSpace(r.company)
			if companyName == "" {
				continue
			}
			companyCode := strings.TrimSpace(r.companyCode)
			if companyCode != "" {
				codeReMatch := regexp.MustCompile(`^[A-Za-z0-9]{3,32}$`)
				if !codeReMatch.MatchString(companyCode) {
					result.Errors = append(result.Errors,
						"第"+itoa(idx+2)+"行：加入码「"+companyCode+"」不合法，已忽略该码")
					companyCode = ""
				}
			}

			// 1) 公司：有码则查码，无码/未命中则新建
			var companyID int64
			found := false
			if companyCode != "" {
				if cid, ok := companyCache[companyCode]; ok {
					companyID = cid
					found = true
				}
			}
			if !found && companyCode != "" {
				if existing, err := store.GetCompanyByCode(ctx, companyCode); err == nil && existing != nil {
					companyID = existing.ID
					companyCache[companyCode] = companyID
					found = true
				}
			}
			if !found {
				for attempt := 0; attempt < 5; attempt++ {
					code := companyCode
					if code == "" {
						code = util.MakeJoinCode()
					}
					company, err := store.CreateCompany(ctx, db.CreateCompanyInput{
						Name:    companyName,
						Code:    code,
						OwnerID: a.ID,
					})
					if err != nil {
						var ae *db.AlreadyExistsError
						if errors.As(err, &ae) {
							if companyCode != "" {
								result.Errors = append(result.Errors,
									"第"+itoa(idx+2)+"行：公司加入码「"+companyCode+"」已被占用，已跳过该公司")
								break
							}
							continue
						}
						result.Errors = append(result.Errors, "第"+itoa(idx+2)+"行：创建公司失败")
						break
					}
					companyID = company.ID
					companyCache[code] = companyID
					if companyCode != "" {
						companyCode = code
					}
					result.Companies++
					found = true
					break
				}
			}
			if !found || companyID == 0 {
				continue
			}

			// 2) 部门：按 公司+部门名 幂等创建
			deptName := strings.TrimSpace(r.department)
			if deptName != "" {
				dk := itoa(int(companyID)) + ":" + deptName
				if _, ok := deptCache[dk]; !ok {
					depts, _ := store.ListDepartments(ctx, companyID)
					var existingID int64
					for _, dp := range depts {
						if dp.Name == deptName {
							existingID = dp.ID
							break
						}
					}
					if existingID != 0 {
						deptCache[dk] = existingID
					} else {
						dept, err := store.CreateDepartment(ctx, db.CreateDepartmentInput{
							CompanyID: companyID,
							Name:      deptName,
						})
						if err != nil {
							result.Errors = append(result.Errors,
								"第"+itoa(idx+2)+"行：创建部门「"+deptName+"」失败")
						} else {
							deptCache[dk] = dept.ID
							result.Departments++
						}
					}
				}
			}

			// 3) 成员：按用户名幂等创建，加入公司并分配部门
			username := strings.TrimSpace(r.username)
			if username == "" {
				continue
			}
			passwordStr := strings.TrimSpace(r.password)
			if passwordStr != "" {
				if len(passwordStr) < 6 || len(passwordStr) > 128 {
					result.Errors = append(result.Errors,
						"第"+itoa(idx+2)+"行：账号「"+username+"」密码需 6-128 位，已跳过")
					continue
				}
			}

			var userID int64
			existingUser, err := store.GetUserByUsername(ctx, username)
			if err == nil && existingUser != nil {
				userID = existingUser.ID
			} else {
				defaultPw := passwordStr
				if defaultPw == "" {
					defaultPw = "mochioa123"
				}
				hash, err := password.HashPassword(defaultPw)
				if err != nil {
					result.Errors = append(result.Errors, "第"+itoa(idx+2)+"行：创建账号「"+username+"」失败")
					continue
				}
				nick := strings.TrimSpace(r.nick)
				if nick == "" {
					nick = username
				}
				created, err := store.CreateUser(ctx, db.CreateUserInput{
					Username:     username,
					PasswordHash: hash,
					Nick:         nick,
				})
				if err != nil {
					result.Errors = append(result.Errors, "第"+itoa(idx+2)+"行：创建账号「"+username+"」失败")
					continue
				}
				userID = created.ID
				result.Users++
			}
			if userID == 0 {
				continue
			}

			// 加入公司
			roleRaw := strings.ToLower(strings.TrimSpace(r.role))
			role := "member"
			if roleRaw == "admin" {
				role = "admin"
			} else if roleRaw == "owner" {
				role = "admin" // TS: owner→admin（导入人是公司 owner）
			}
			cur, _ := store.GetMemberRole(ctx, companyID, userID)
			if cur == "" {
				if err := store.AddMember(ctx, companyID, userID, role); err != nil {
					result.Errors = append(result.Errors,
						"第"+itoa(idx+2)+"行：加入公司「"+companyName+"」失败")
				} else {
					result.Memberships++
					_ = srv.EmitTo(util.UserRoom(userID), "company:added", map[string]any{"companyId": companyID})
					_ = srv.EmitTo(util.CompanyRoom(companyID), "company:updated", map[string]any{"companyId": companyID})
				}
			} else {
				result.Skipped++
			}

			// 分配部门
			if deptName != "" {
				dk := itoa(int(companyID)) + ":" + deptName
				if deptID, ok := deptCache[dk]; ok {
					_ = store.AssignDepartment(ctx, deptID, userID)
				}
			}
		}

		logger.Info(mod, "CSV 导入完成",
			"companies", result.Companies, "departments", result.Departments,
			"users", result.Users, "memberships", result.Memberships,
			"skipped", result.Skipped, "errors", len(result.Errors),
			"userId", a.ID)

		ack(util.OK(map[string]any{
			"ok":          result.Ok,
			"companies":   result.Companies,
			"departments": result.Departments,
			"users":       result.Users,
			"memberships": result.Memberships,
			"skipped":     result.Skipped,
			"errors":      result.Errors,
		}))
	})
}

// itoa 简易 int→string（避免在循环中引入 strconv 包名冲突）。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
