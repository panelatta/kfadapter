package state

import (
	"context"
	"database/sql/driver"
	"fmt"

	"modernc.org/sqlite"
)

// sqliteStateConnector applies the byte limit to every physical connection,
// including replacements opened by database/sql. max_page_count is connection
// local, so setting it once on *sql.DB would leave replacement connections
// unbounded. SQLite rejects growth within the writing transaction, preserving
// the previous state instead of committing a database our readers reject.
type sqliteStateConnector struct {
	dsn    string
	driver sqlite.Driver
}

func (c *sqliteStateConnector) Driver() driver.Driver { return &c.driver }

func (c *sqliteStateConnector) Connect(ctx context.Context) (driver.Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	conn, err := c.driver.Open(c.dsn)
	if err != nil {
		return nil, err
	}
	query := conn.(sqlite.ExecQuerierContext)
	if err := limitSQLiteStateSize(ctx, query); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return conn, nil
}

func limitSQLiteStateSize(ctx context.Context, conn sqlite.ExecQuerierContext) error {
	pageSize, err := sqlitePragmaInt(ctx, conn, "PRAGMA main.page_size")
	if err != nil {
		return err
	}
	// Existing databases may use a different page size from SQLite's default.
	if pageSize < 512 || pageSize > 65536 || pageSize&(pageSize-1) != 0 {
		return fmt.Errorf("invalid SQLite page size %d", pageSize)
	}
	maximum := int64(maxSQLiteStateBytes) / pageSize
	actual, err := sqlitePragmaInt(ctx, conn, fmt.Sprintf("PRAGMA main.max_page_count = %d", maximum))
	if err != nil {
		return err
	}
	// SQLite cannot lower max_page_count below the current database size.
	if actual != maximum {
		return fmt.Errorf("SQLite state exceeds maximum size")
	}
	return nil
}

func sqlitePragmaInt(ctx context.Context, conn sqlite.ExecQuerierContext, statement string) (int64, error) {
	rows, err := conn.QueryContext(ctx, statement, nil)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	values := make([]driver.Value, 1)
	if err := rows.Next(values); err != nil {
		return 0, err
	}
	value, ok := values[0].(int64)
	if !ok {
		return 0, fmt.Errorf("invalid SQLite integer result")
	}
	return value, nil
}
