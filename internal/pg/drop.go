package pg

import (
	"context"
	"errors"
	"fmt"

	"dboss/internal/fault"

	"github.com/jackc/pgx/v5"
)

// ErrDropConfirm is returned when a drop is missing the database name as confirmation.
var ErrDropConfirm = fault.Invalid(errors.New("dropping a database requires its name as confirmation"))

// reservedDatabases can never be dropped through dboss.
var reservedDatabases = map[string]bool{"postgres": true, "template0": true, "template1": true}

// DropDatabase removes one database through the maintenance connection. It refuses the reserved
// databases and requires the caller to repeat the name as confirmation.
func (s *Service) DropDatabase(ctx context.Context, database, confirm string) error {
	if database == "" {
		return fault.Invalidf("database name is required")
	}
	if database != confirm {
		return ErrDropConfirm
	}
	if reservedDatabases[database] {
		return fault.Invalidf("%s is a reserved database and cannot be dropped", database)
	}
	connConfig, err := s.connection(ctx)
	if err != nil {
		return err
	}
	if err := maintenanceExec(ctx, connConfig, "DROP DATABASE IF EXISTS "+pgx.Identifier{database}.Sanitize()+" WITH (FORCE)"); err != nil {
		return fmt.Errorf("drop database %s: %w", database, err)
	}
	return nil
}

// maintenanceExec runs one statement on the first of postgres or template1 that accepts a
// connection, since a database cannot be dropped from inside itself.
func maintenanceExec(ctx context.Context, connConfig *pgx.ConnConfig, statement string) error {
	var lastErr error
	for _, maintenance := range []string{"postgres", "template1"} {
		clone := connConfig.Copy()
		clone.Database = maintenance
		conn, err := pgx.ConnectConfig(ctx, clone)
		if err != nil {
			lastErr = err
			continue
		}
		_, err = conn.Exec(ctx, statement)
		_ = conn.Close(ctx)
		if err == nil {
			return nil
		}
		lastErr = err
	}
	return lastErr
}
