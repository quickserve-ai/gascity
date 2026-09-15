package doltpool

import (
	"database/sql"
	"fmt"
	"net"
	"time"

	mysql "github.com/go-sql-driver/mysql"
)

// OpenPrivate opens a Dolt handle that the shared registry never holds: every
// call returns a new *sql.DB, and the caller owns it and must Close it. That is
// the one reason to reach for it — a long-lived handle its owner has to be able
// to retire and re-dial, where closing a shared Open handle would poison the
// pool entry of every other caller dialing the same endpoint. Anything that
// opens per operation belongs on Open: a private handle per call is exactly the
// Open+Close churn this package exists to prevent.
//
// timeout bounds the dial and each read and write on the handle's connections.
// Unlike Open's DSN, ParseTime stays off, so DATETIME columns scan as the
// driver's raw bytes.
func OpenPrivate(host, port, user, password, database string, timeout time.Duration) (*sql.DB, error) {
	db, err := sql.Open("mysql", privateDSN(host, port, user, password, database, timeout))
	if err != nil {
		return nil, fmt.Errorf("opening private dolt connection to %s:%s/%s: %w", host, port, database, err)
	}
	return db, nil
}

// privateDSN builds the go-sql-driver DSN for one OpenPrivate handle.
func privateDSN(host, port, user, password, database string, timeout time.Duration) string {
	cfg := mysql.NewConfig()
	cfg.User = user
	cfg.Passwd = password
	cfg.Net = "tcp"
	cfg.Addr = net.JoinHostPort(host, port)
	cfg.DBName = database
	cfg.Timeout = timeout
	cfg.ReadTimeout = timeout
	cfg.WriteTimeout = timeout
	cfg.AllowNativePasswords = true
	return cfg.FormatDSN()
}
