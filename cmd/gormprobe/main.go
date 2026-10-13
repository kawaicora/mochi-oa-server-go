// 临时探针：验证 GORM 关键用法（用完即删）。
package main

import (
	"fmt"
	"time"

	gormmysql "gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	gormlogger "gorm.io/gorm/logger"

	"mochi-oa-server-go/internal/types"
)

func must(err error) {
	if err != nil {
		panic(err)
	}
}

func main() {
	rootDSN := "root:verify_pw@tcp(127.0.0.1:3306)/?charset=utf8mb4&parseTime=true&loc=UTC"
	root, err := gorm.Open(gormmysql.Open(rootDSN), &gorm.Config{Logger: gormlogger.Default.LogMode(gormlogger.Info)})
	if err != nil {
		panic(err)
	}
	must(root.Exec("CREATE DATABASE IF NOT EXISTS gorm_probe").Error)
	must(root.Exec("DROP TABLE IF EXISTS gorm_probe.t").Error)
	must(root.Exec(`CREATE TABLE gorm_probe.t (
		id BIGINT PRIMARY KEY AUTO_INCREMENT,
		company_id BIGINT NOT NULL,
		user_id BIGINT NOT NULL,
		role VARCHAR(32) NOT NULL,
		joined_at DATETIME(3) NOT NULL,
		note VARCHAR(64) NULL,
		email VARCHAR(64) NOT NULL DEFAULT ''
	)`).Error)
	must(root.Exec(`INSERT INTO gorm_probe.t (company_id,user_id,role,joined_at,note,email) VALUES (1,1,'owner',NOW(3),'n1','a@b.c')`).Error)

	dsn := "root:verify_pw@tcp(127.0.0.1:3306)/gorm_probe?charset=utf8mb4&parseTime=true&loc=UTC"
	db, err := gorm.Open(gormmysql.Open(dsn), &gorm.Config{
		Logger:                 gormlogger.Default.LogMode(gormlogger.Info),
		SkipDefaultTransaction: true,
	})
	must(err)

	// 1) 无模型 Delete 的两种写法
	_ = db.Exec("INSERT INTO t (company_id,user_id,role,joined_at) VALUES (9,9,'x',NOW(3))")
	r1 := db.Table("t").Where("company_id = ? AND user_id = ?", 9, 9).Delete(nil)
	fmt.Println("DELETE(nil) err=", r1.Error, "rows=", r1.RowsAffected)

	_ = db.Exec("INSERT INTO t (company_id,user_id,role,joined_at) VALUES (8,8,'x',NOW(3))")
	r2 := db.Table("t").Where("company_id = ? AND user_id = ?", 8, 8).Delete(&struct{}{})
	fmt.Println("DELETE(&struct{}{}) err=", r2.Error, "rows=", r2.RowsAffected)

	// 2) 无模型 Update 的 RowsAffected + map 更新
	r3 := db.Table("t").Where("company_id = ? AND user_id = ?", 1, 1).Updates(map[string]any{"role": "admin"})
	fmt.Println("UPDATE map err=", r3.Error, "rows=", r3.RowsAffected)
	r4 := db.Table("t").Where("company_id = ? AND user_id = ?", 999, 999).Updates(map[string]any{"role": "admin"})
	fmt.Println("UPDATE map(no row) err=", r4.Error, "rows=", r4.RowsAffected)

	// 3) 把 email 置空串（零值）验证 map 写法可行
	r5 := db.Table("t").Where("company_id = ? AND user_id = ?", 1, 1).Updates(map[string]any{"email": ""})
	fmt.Println("UPDATE email='' err=", r5.Error, "rows=", r5.RowsAffected)
	var got []string
	must(db.Table("t").Where("company_id = 1").Pluck("email", &got).Error)
	fmt.Println("email after=", got)

	// 4) 无名主键 struct 的 Take / 命名策略映射
	var row struct {
		CompanyID int64  `gorm:"column:company_id"`
		Role      string `gorm:"column:role"`
	}
	e := db.Table("t").Where("company_id = ?", 1).Take(&row).Error
	fmt.Println("Take err=", e, "row=", row)
	e2 := db.Table("t").Where("company_id = ?", 777).Take(&row).Error
	fmt.Println("Take(missing) err=", e2, "isNotFound=", e2 == gorm.ErrRecordNotFound)

	// 5) types.CompanyMemberRow 默认命名策略映射（无 gorm tag）
	var cm types.CompanyMemberRow
	e3 := db.Table("company_members").Where("company_id = 1").Take(&cm).Error
	fmt.Println("types.CompanyMemberRow (mochioa? use probe table) err=", e3)

	// 6) Pluck 无行时的错误
	var role string
	e4 := db.Table("t").Where("company_id = ?", 777).Pluck("role", &role).Error
	fmt.Println("Pluck(missing) err=", e4, "isNotFound=", e4 == gorm.ErrRecordNotFound, "role=", role)

	// 7) NULL 扫描到 string 是否报错
	_ = db.Exec("INSERT INTO t (company_id,user_id,role,joined_at,note) VALUES (5,5,'x',NOW(3),NULL)")
	var n struct {
		Note *string `gorm:"column:note"`
	}
	e5 := db.Table("t").Where("company_id = 5").Select("note").Take(&n).Error
	fmt.Println("NULL->*string err=", e5, "note=", n.Note)

	var n2 struct {
		Note string `gorm:"column:note"`
	}
	e6 := db.Table("t").Where("company_id = 5").Select("note").Take(&n2).Error
	fmt.Println("NULL->string err=", e6)

	// 8) ON CONFLICT DO NOTHING 幂等插入
	ins := db.Table("t").Clauses(clause.OnConflict{DoNothing: true}).Create(map[string]any{
		"company_id": 1, "user_id": 1, "role": "member", "joined_at": time.Now().UTC(),
	})
	fmt.Println("INSERT IGNORE err=", ins.Error, "rows=", ins.RowsAffected)

	// 9) created_at 自动填充行为（map 插入是否被自动加 created_at）
	fmt.Println("done")
}
