// Package boot exposes the parts of the Wings boot sequence that live in
// internal packages so that Wings can be embedded as a library by other
// daemons. The Wings binary itself continues to use the internal packages
// directly.
package boot

import (
	"context"

	"github.com/go-co-op/gocron/v2"

	"github.com/pelican/wings/internal/cron"
	"github.com/pelican/wings/internal/database"
	"github.com/pelican/wings/server"
)

// InitializeDatabase configures the local SQLite activity database for Wings
// and ensures that the models have been fully migrated. It must be called
// exactly once per process, after the configuration has been set.
func InitializeDatabase() error {
	return database.Initialize()
}

// Scheduler configures the internal cron system (activity and SFTP event
// shipping) for the given server manager and returns the scheduler. The caller
// starts it. It must be called at most once per process.
func Scheduler(ctx context.Context, m *server.Manager) (gocron.Scheduler, error) {
	return cron.Scheduler(ctx, m)
}
