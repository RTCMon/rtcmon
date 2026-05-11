// Package migrate wraps golang-migrate to apply and roll back SQL migrations
// embedded in the migrations package. SQL files are embedded at compile time
// so the binary requires no external files at runtime.
package migrate

import (
	"context"
	"errors"
	"fmt"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"

	migrations "github.com/RTCMon/rtcmon/migrations"
)

// newMigrator creates a configured migrate.Migrate instance.
// The caller is responsible for calling m.Close() when done.
func newMigrator(dbURL string) (*migrate.Migrate, error) {
	src, err := iofs.New(migrations.FS, ".")
	if err != nil {
		return nil, fmt.Errorf("migrate: load source: %w", err)
	}

	// Normalize scheme for golang-migrate pgx/v5: it expects pgx5:// prefix
	// when utilizing the modern pgx/v5 driver registration.
	migURL := dbURL
	if len(dbURL) > 11 && dbURL[:11] == "postgres://" {
		migURL = "pgx5://" + dbURL[11:]
	} else if len(dbURL) > 13 && dbURL[:13] == "postgresql://" {
		migURL = "pgx5://" + dbURL[13:]
	}

	m, err := migrate.NewWithSourceInstance("iofs", src, migURL)
	if err != nil {
		return nil, fmt.Errorf("migrate: connect: %w", err)
	}

	return m, nil
}

// Up applies all pending migrations. It is idempotent: if the schema is
// already up to date, Up returns nil (ErrNoChange is swallowed).
func Up(_ context.Context, dbURL string) error {
	m, err := newMigrator(dbURL)
	if err != nil {
		return err
	}
	defer m.Close()

	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("migrate up: %w", err)
	}

	return nil
}

// Down rolls back exactly `steps` migrations. Pass 0 to roll back all applied
// migrations. It is safe to call on a schema that has already been fully rolled
// back (ErrNoChange is swallowed).
func Down(_ context.Context, dbURL string, steps int) error {
	m, err := newMigrator(dbURL)
	if err != nil {
		return err
	}
	defer m.Close()

	var migrateErr error
	if steps == 0 {
		migrateErr = m.Down()
	} else {
		migrateErr = m.Steps(-steps)
	}

	if migrateErr != nil && !errors.Is(migrateErr, migrate.ErrNoChange) {
		return fmt.Errorf("migrate down: %w", migrateErr)
	}

	return nil
}
