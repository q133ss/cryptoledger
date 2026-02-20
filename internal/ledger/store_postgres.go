package ledger

import (
	"context"
	"fmt"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
)

// OpenPostgres connects to a database provisioned with deploy/postgres/schema.sql.
// The runtime login inherits ledger_service and cannot change the schema.
func OpenPostgres(ctx context.Context, dsn string) (*Store, error) {
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("invalid PostgreSQL configuration")
	}
	config.ConnConfig.RuntimeParams["search_path"] = "finance,pg_catalog"
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, err
	}
	db := stdlib.OpenDBFromPool(pool)
	s := &Store{db: db, postgres: true, closePool: pool.Close}
	if _, err = db.ExecContext(ctx, "SELECT tx_id FROM transactions LIMIT 0"); err != nil {
		s.Close()
		return nil, fmt.Errorf("check ledger schema: %w", err)
	}
	return s, nil
}
