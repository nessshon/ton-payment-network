package db

import (
	"context"
	"fmt"
	"github.com/xssnick/ton-payment-network/pkg/log"
)

type Migration func(ctx context.Context, db *DB) error

var Migrations = []Migration{}

func RunMigrations(db *DB) error {
	version, err := db.GetMigrationVersion(context.Background())
	if err != nil {
		return fmt.Errorf("failed to get migration version: %w", err)
	}

	if version < len(Migrations) {
		log.Info().Msgf("required migrations from %d to %d, backuping database...", version, len(Migrations))
		if err = db.storage.Backup(); err != nil {
			return fmt.Errorf("failed to backup db: %w", err)
		}
		log.Info().Msg("backup completed, starting migrations")
	}

	for i := version; i < len(Migrations); i++ {
		log.Info().Msgf("running migration %d", i+1)
		err := db.Transaction(context.Background(), func(ctx context.Context) error {
			if err := Migrations[i](ctx, db); err != nil {
				return fmt.Errorf("failed to run migration %d: %w", i, err)
			}

			err := db.SetMigrationVersion(ctx, i+1)
			if err != nil {
				return fmt.Errorf("failed to set migration version: %w", err)
			}
			return nil
		})
		if err != nil {
			return err
		}
		log.Info().Msgf("migration %d done", i+1)
	}

	return nil
}
