// 临时工具：探查 MySQL 数据库，定位测试库与 cid=9 当天打卡记录。跑完即删，不入 git。
package main

import (
	"database/sql"
	"fmt"
	"os"
	"time"

	_ "github.com/go-sql-driver/mysql"
)

func main() {
	pass := os.Getenv("MYSQL_PASSWORD")
	dsn := fmt.Sprintf("root:%s@tcp(127.0.0.1:3306)/?parseTime=true&charset=utf8mb4", pass)
	db, err := sql.Open("mysql", dsn)
	if err != nil { fmt.Println("open err:", err); os.Exit(1) }
	defer db.Close()
	if err := db.Ping(); err != nil { fmt.Println("ping err:", err); os.Exit(1) }
	rows, err := db.Query("SHOW DATABASES")
	if err != nil { fmt.Println("show db err:", err); os.Exit(1) }
	var dbs []string
	for rows.Next() {
		var d string
		rows.Scan(&d)
		dbs = append(dbs, d)
	}
	rows.Close()
	fmt.Println("DATABASES:", dbs)
	for _, d := range dbs {
		var cnt int
		err := db.QueryRow(fmt.Sprintf("SELECT COUNT(*) FROM `%s`.users WHERE username='v10_admin'", d)).Scan(&cnt)
		if err == nil && cnt > 0 {
			fmt.Printf(">>> v10_admin 在库 %s (cnt=%d)\n", d, cnt)
			var uid int64
			db.QueryRow(fmt.Sprintf("SELECT id FROM `%s`.users WHERE username='v10_admin'", d)).Scan(&uid)
			fmt.Println("   uid=", uid)
			// 公司
			crows, _ := db.Query(fmt.Sprintf("SELECT id, name FROM `%s`.companies", d))
			for crows.Next() {
				var id int64; var name string
				crows.Scan(&id, &name)
				fmt.Printf("   company: id=%d name=%s\n", id, name)
			}
			crows.Close()
			// 今天 checkins（cid=9, uid=28）
			cr, _ := db.Query(fmt.Sprintf("SELECT id, user_id, check_date, check_time, ip FROM `%s`.checkins WHERE company_id=9 ORDER BY check_time ASC", d))
			for cr.Next() {
				var id int64; var uid2 int64; var dt string; var tm time.Time; var ip *string
				cr.Scan(&id, &uid2, &dt, &tm, &ip)
				ipS := ""
				if ip != nil { ipS = *ip }
				fmt.Printf("   checkin id=%d uid=%d date=%s time=%s ip=%s\n", id, uid2, dt, tm.Format("15:04:05"), ipS)
			}
			cr.Close()
		}
	}
}
