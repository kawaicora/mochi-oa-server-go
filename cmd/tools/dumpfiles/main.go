// dumpfiles 只读诊断工具：直连 MySQL 打印 files 表最近记录（uuid/filename/mime/size/url/hash/using_file_id）。
// 仅用于线上排查文件复用/下载问题，不写库。
package main

import (
	"database/sql"
	"fmt"
	"os"

	_ "github.com/go-sql-driver/mysql"
)

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func main() {
	dsn := fmt.Sprintf("%s:%s@tcp(%s:%s)/%s?charset=utf8mb4&parseTime=true&loc=UTC",
		env("MYSQL_USER", ""), env("MYSQL_PASSWORD", ""),
		env("MYSQL_HOST", "127.0.0.1"), env("MYSQL_PORT", "3306"), env("MYSQL_DATABASE", ""))
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		fmt.Println("open:", err)
		os.Exit(1)
	}
	defer db.Close()

	rows, err := db.Query("SELECT uuid, filename, mime, size, storage, url, hash, using_file_id FROM files ORDER BY created_at DESC LIMIT 30")
	if err != nil {
		fmt.Println("query:", err)
		os.Exit(1)
	}
	defer rows.Close()

	for rows.Next() {
		var uuid, filename, mime, storage, url string
		var size int64
		var usingFileID, hash sql.NullString
		if err := rows.Scan(&uuid, &filename, &mime, &size, &storage, &url, &hash, &usingFileID); err != nil {
			fmt.Println("scan:", err)
			os.Exit(1)
		}
		fmt.Printf("uuid=%s filename=%q mime=%s size=%d storage=%s url=%s using=%q hash=%.12s\n",
			uuid, filename, mime, size, storage, url, usingFileID.String, hash.String)
	}
}
