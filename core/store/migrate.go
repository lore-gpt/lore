package store

import (
	"context"
	"database/sql"
	"fmt"

	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver used by goose
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"

	"github.com/lore-gpt/lore/core/store/migrations"
)

// RunMigrations applies all pending goose migrations from the embedded FS.
//
// It opens its own short-lived database/sql connection (goose needs *sql.DB) via
// the pgx stdlib driver, and uses goose's provider API with a Postgres session
// locker: a session-level advisory lock serializes concurrent migrators, so two
// processes booting against a fresh database can't race the same migration. Safe
// to call on every boot — it is a no-op once the schema is current.
func RunMigrations(ctx context.Context, dsn string) error {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return fmt.Errorf("open db for migrations: %w", err)
	}
	defer func() { _ = db.Close() }()

	locker, err := lock.NewPostgresSessionLocker()
	if err != nil {
		return fmt.Errorf("create session locker: %w", err)
	}
	provider, err := goose.NewProvider(goose.DialectPostgres, db, migrations.FS,
		goose.WithSessionLocker(locker))
	if err != nil {
		return fmt.Errorf("create goose provider: %w", err)
	}
	if _, err := provider.Up(ctx); err != nil {
		return fmt.Errorf("run migrations: %w", err)
	}
	return nil
}
