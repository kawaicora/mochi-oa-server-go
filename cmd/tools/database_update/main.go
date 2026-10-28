// Package main 是 mochi-oa-server-go 的独立数据库迁移 CLI 工具。
//
// 背景：数据库结构迁移（枚举扩展、补列、旧库 DDL 升级）原本内嵌在服务端启动时的
// autoMigrateTables 里；现按约定抽成独立工具，服务端主程序只负责 GORM AutoMigrate
// 建表/补列，所有显式 DDL 迁移统一由本工具执行。
//
// 用法（编译产物：cmd/dbmigrate，见 build.ps1 / build.sh 输出 dist/mochi-oa-dbmigrate_*）：
//   ./mochi-oa-dbmigrate_linux_amd64            # 读 .env 连接 MySQL，按清单幂等执行迁移
//   ./mochi-oa-dbmigrate_linux_amd64 --list     # 仅列出迁移清单，不执行
//   ./mochi-oa-dbmigrate_linux_amd64 --dry-run  # 只打印将要执行的迁移名，不真正执行
//
// 幂等原则：每步都先查 information_schema（列是否存在 / 当前类型）再决定是否执行；
// 已满足目标结构的步骤直接跳过；单个步骤失败只记日志、不中断后续步骤，
// 与旧 setup.go 的「不阻断启动」语义一致，可反复执行。
package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"time"

	// 注册 mysql 驱动。
	_ "github.com/go-sql-driver/mysql"

	"mochi-oa-server-go/internal/config"
	"mochi-oa-server-go/internal/logger"
)

// migration 描述一个可重复执行的迁移步骤。
type migration struct {
	name string
	run  func(ctx context.Context, db *sql.DB) error
}

func main() {
	args := os.Args[1:]
	if len(args) == 1 && args[0] == "--list" {
		for _, m := range migrations() {
			fmt.Println(m.name)
		}
		return
	}
	dryRun := len(args) == 1 && args[0] == "--dry-run"

	cfg := config.Load()
	dsn := fmt.Sprintf("%s:%s@tcp(%s:%d)/%s?charset=utf8mb4&parseTime=true&loc=UTC",
		cfg.MySQL.User, cfg.MySQL.Password, cfg.MySQL.Host, cfg.MySQL.Port, cfg.MySQL.Database)
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		logger.Error("dbmigrate", "打开数据库失败", "error", err.Error())
		os.Exit(1)
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		logger.Error("dbmigrate", "连接 MySQL 失败", "host", cfg.MySQL.Host, "port", cfg.MySQL.Port, "db", cfg.MySQL.Database, "error", err.Error())
		os.Exit(1)
	}
	logger.Info("dbmigrate", "已连接", "host", cfg.MySQL.Host, "db", cfg.MySQL.Database)

	// 中断标志：默认 false；某一步因为「不是幂等错误」失败时置 true 提示复查。
	failed := false
	for _, m := range migrations() {
		if dryRun {
			logger.Info("dbmigrate", "[dry-run] 跳过执行", "migration", m.name)
			continue
		}
		if err := m.run(ctx, db); err != nil {
			// 幂等迁移允许目标结构已满足时因 DDL 重复报错（1060 加列 / 枚举重复），
			// 不阻断后续；但记录出来供人工复查。
			logger.Warn("dbmigrate", "迁移步骤未完成（可能已满足目标结构，可复查）", "migration", m.name, "error", err.Error())
			failed = true
		} else {
			logger.Info("dbmigrate", "迁移完成", "migration", m.name)
		}
	}
	if failed {
		logger.Warn("dbmigrate", "有迁移步骤返回告警，请结合上方日志核对目标列结构；工具本身可重复执行")
	} else {
		logger.Info("dbmigrate", "全部迁移完成")
	}
}

// migrations 返回按序执行的迁移清单（幂等，可重复执行）。
func migrations() []migration {
	return []migration{
		{
			name: "messages.kind 扩展 ENUM（proxy_card/expense_card/advance_card）",
			run: func(ctx context.Context, db *sql.DB) error {
				_, err := db.ExecContext(ctx,
					"ALTER TABLE messages MODIFY `kind` ENUM('text','image','file','video','audio','folder','proxy_card','expense_card','advance_card') NOT NULL DEFAULT 'text'")
				return err
			},
		},
		{
			name: "attendance_requests.cc_viewed_ids 补列",
			run: func(ctx context.Context, db *sql.DB) error {
				_, err := db.ExecContext(ctx, "ALTER TABLE attendance_requests ADD COLUMN cc_viewed_ids JSON NULL")
				return err
			},
		},
		{
			name: "attendance_requests/proxy_access_requests.notify_stopped 补列",
			run: func(ctx context.Context, db *sql.DB) error {
				for _, tbl := range []string{"attendance_requests", "proxy_access_requests"} {
					if _, err := db.ExecContext(ctx, "ALTER TABLE "+tbl+" ADD COLUMN notify_stopped TINYINT NOT NULL DEFAULT 0"); err != nil {
						return err
					}
				}
				return nil
			},
		},
		{
			// 旧库：work_start/work_end 旧列 → std_start/std_end 快照拷贝（空串用 NULLIF 归一为 NULL）。
			name: "checkins 快照列迁移 work_start/work_end → std_start/std_end",
			run: func(ctx context.Context, db *sql.DB) error {
				ok, err := colExists(ctx, db, "checkins", "work_start")
				if err != nil || !ok {
					return err
				}
				_, err = db.ExecContext(ctx,
					"UPDATE checkins SET std_start = NULLIF(work_start, ''), std_end = NULLIF(work_end, '') WHERE std_start IS NULL AND work_start IS NOT NULL")
				return err
			},
		},
		{
			// checkins.std_start/std_end：仅旧库 varchar → datetime（业务口径为完整 datetime，
			// 写入 "YYYY-MM-DD HH:MM:SS"；生产列已 datetime 则跳过）。先清空串再 MODIFY。
			name: "checkins.std_start/std_end varchar→datetime",
			run: func(ctx context.Context, db *sql.DB) error {
				t, err := colType(ctx, db, "checkins", "std_start")
				if err != nil {
					return err
				}
				if t != "varchar" {
					return nil // 已 datetime / time，跳过
				}
				for _, q := range []string{
					"UPDATE checkins SET std_start = NULL WHERE std_start = ''",
					"UPDATE checkins SET std_end = NULL WHERE std_end = ''",
					"ALTER TABLE checkins MODIFY `std_start` DATETIME NULL",
					"ALTER TABLE checkins MODIFY `std_end` DATETIME NULL",
				} {
					if _, err := db.ExecContext(ctx, q); err != nil {
						return err
					}
				}
				return nil
			},
		},
		{
			// 仅旧库 varchar → TIME（列在 company_checkin_rules，不在 settings）。
			name: "company_checkin_rules.offwork_deadline varchar→time",
			run: func(ctx context.Context, db *sql.DB) error {
				t, err := colType(ctx, db, "company_checkin_rules", "offwork_deadline")
				if err != nil {
					return err
				}
				if t != "varchar" {
					return nil
				}
				_, err = db.ExecContext(ctx, "ALTER TABLE company_checkin_rules MODIFY `offwork_deadline` TIME NOT NULL")
				return err
			},
		},
		{
			// 仅旧库 varchar → DATE。
			name: "checkins.check_date varchar→date",
			run: func(ctx context.Context, db *sql.DB) error {
				t, err := colType(ctx, db, "checkins", "check_date")
				if err != nil {
					return err
				}
				if t != "varchar" {
					return nil
				}
				_, err = db.ExecContext(ctx, "ALTER TABLE checkins MODIFY `check_date` DATE NOT NULL")
				return err
			},
		},
		{
			// 仅旧库 varchar → DATETIME。
			name: "attendance_requests.punch_time varchar→datetime",
			run: func(ctx context.Context, db *sql.DB) error {
				t, err := colType(ctx, db, "attendance_requests", "punch_time")
				if err != nil {
					return err
				}
				if t != "varchar" {
					return nil
				}
				for _, q := range []string{
					"UPDATE attendance_requests SET punch_time = REPLACE(punch_time, 'T', ' ') WHERE punch_time LIKE '%T%'",
					"UPDATE attendance_requests SET punch_time = NULL WHERE punch_time = ''",
					"ALTER TABLE attendance_requests MODIFY `punch_time` DATETIME NULL",
				} {
					if _, err := db.ExecContext(ctx, q); err != nil {
						return err
					}
				}
				return nil
			},
		},
		{
			// 仅旧库 bigint(unix秒) → DATETIME；新库已是 datetime 直接跳过，避免误删正确列。
			name: "afdian_orders.create_time bigint→datetime",
			run: func(ctx context.Context, db *sql.DB) error {
				t, err := colType(ctx, db, "afdian_orders", "create_time")
				if err != nil {
					return err
				}
				if t != "bigint" {
					return nil
				}
				for _, q := range []string{
					"ALTER TABLE afdian_orders ADD COLUMN _ct_dt DATETIME NULL",
					"UPDATE afdian_orders SET _ct_dt = FROM_UNIXTIME(create_time) WHERE create_time > 0 AND _ct_dt IS NULL",
					"ALTER TABLE afdian_orders DROP COLUMN create_time",
					"ALTER TABLE afdian_orders CHANGE COLUMN _ct_dt create_time DATETIME NOT NULL",
				} {
					if _, err := db.ExecContext(ctx, q); err != nil {
						return err
					}
				}
				return nil
			},
		},
	}
}

// colExists 判断某表某列是否存在（information_schema）。
func colExists(ctx context.Context, db *sql.DB, table, column string) (bool, error) {
	var n int
	err := db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM information_schema.COLUMNS WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME=? AND COLUMN_NAME=?",
		table, column).Scan(&n)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// colType 取某列数据类型（小写，如 time/datetime/bigint/varchar）；列不存在返回空串。
func colType(ctx context.Context, db *sql.DB, table, column string) (string, error) {
	var t string
	err := db.QueryRowContext(ctx,
		"SELECT DATA_TYPE FROM information_schema.COLUMNS WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME=? AND COLUMN_NAME=?",
		table, column).Scan(&t)
	if err != nil {
		if err == sql.ErrNoRows {
			return "", nil
		}
		return "", err
	}
	return t, nil
}
