package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"regexp"
	"time"

	"github.com/go-sql-driver/mysql"
)

// ChangeEntry is one thing someone did to an access point through the app:
// applied settings, ran console commands, or viewed a saved secret. Commands
// are stored with secrets masked.
type ChangeEntry struct {
	At       time.Time `json:"at"`
	User     string    `json:"user"`
	APID     string    `json:"ap"`
	Action   string    `json:"action"` // apply, console, view_secret, disconnect
	Detail   string    `json:"detail,omitempty"`
	Commands []string  `json:"commands,omitempty"`
	OK       bool      `json:"ok"`
	Error    string    `json:"error,omitempty"`
}

// changeLog keeps ChangeEntries in MariaDB. A nil *changeLog (database
// disabled) records nothing, so callers don't need to check.
type changeLog struct {
	db *sql.DB
}

var changes *changeLog

func openChangeLog(c DatabaseConfig) (*changeLog, error) {
	mc := mysql.NewConfig()
	mc.User = c.User
	mc.Passwd = c.Password
	mc.Net = "tcp"
	mc.Addr = fmt.Sprintf("%s:%d", c.Host, c.Port)
	mc.DBName = c.Name
	mc.ParseTime = true
	mc.Loc = time.UTC
	mc.Params = map[string]string{"time_zone": "'+00:00'"}

	db, err := sql.Open("mysql", mc.FormatDSN())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(4)
	db.SetConnMaxLifetime(30 * time.Minute)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("connect to %s as %s: %w", mc.Addr, c.User, err)
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS change_log (
		id         BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
		created_at DATETIME NOT NULL,
		user       VARCHAR(64)  NOT NULL,
		ap_id      VARCHAR(64)  NOT NULL,
		action     VARCHAR(32)  NOT NULL,
		detail     VARCHAR(255) NOT NULL DEFAULT '',
		commands   TEXT NULL,
		ok         BOOLEAN NOT NULL,
		error      VARCHAR(512) NOT NULL DEFAULT '',
		KEY idx_ap_created (ap_id, created_at)
	) ENGINE=InnoDB`); err != nil {
		db.Close()
		return nil, fmt.Errorf("create change_log: %w", err)
	}
	return &changeLog{db: db}, nil
}

// Record saves e. A failure is logged but never fails the request: the change
// on the AP has already happened.
func (l *changeLog) Record(e ChangeEntry) {
	if l == nil {
		return
	}
	var cmds any // NULL when there are none
	if len(e.Commands) > 0 {
		b, _ := json.Marshal(e.Commands)
		cmds = string(b)
	}
	if _, err := l.db.Exec(`INSERT INTO change_log (created_at, user, ap_id, action, detail, commands, ok, error)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		time.Now().UTC(), truncate(e.User, 64), truncate(e.APID, 64), e.Action,
		truncate(e.Detail, 255), cmds, e.OK, truncate(e.Error, 512)); err != nil {
		log.Printf("⚠️  could not write change log: %v", err)
	}
}

// Recent returns the newest n entries for an access point.
func (l *changeLog) Recent(apID string, n int) ([]ChangeEntry, error) {
	rows, err := l.db.Query(`SELECT created_at, user, ap_id, action, detail, commands, ok, error
		FROM change_log WHERE ap_id = ? ORDER BY id DESC LIMIT ?`, apID, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	entries := []ChangeEntry{}
	for rows.Next() {
		var e ChangeEntry
		var cmds sql.NullString
		if err := rows.Scan(&e.At, &e.User, &e.APID, &e.Action, &e.Detail, &cmds, &e.OK, &e.Error); err != nil {
			return nil, err
		}
		if cmds.Valid {
			json.Unmarshal([]byte(cmds.String), &e.Commands)
		}
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// consoleSecret matches the value after CLI keywords that take a secret, so
// console commands typed by hand can be logged without it.
var consoleSecret = regexp.MustCompile(`(?i)\b(passphrase|password|secret|key|read-community|write-community|community)(\s+)("[^"]*"|\S+)`)

// MaskConsoleSecrets hides secret values in raw console commands.
func MaskConsoleSecrets(cmds []string) []string {
	masked := make([]string, len(cmds))
	for i, c := range cmds {
		masked[i] = consoleSecret.ReplaceAllString(c, "$1$2********")
	}
	return masked
}
