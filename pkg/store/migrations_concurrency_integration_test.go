package store

import (
	"context"
	"sync"
	"testing"

	"softpos/migrations"
	"softpos/pkg/store/pgtest"
)

// Several server replicas booting at once against a fresh database must all
// succeed: migrations are serialized instead of racing on schema_migrations.
func TestPGIntegration_MigrationsConcurrentStartup(t *testing.T) {
	db := pgtest.Open(t)
	ctx := context.Background()
	const replicas = 6
	var wg sync.WaitGroup
	errs := make(chan error, replicas)
	start := make(chan struct{})
	for i := 0; i < replicas; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if err := RunMigrationsFS(ctx, db, migrations.FS); err != nil {
				errs <- err
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent RunMigrationsFS failed: %v", err)
	}
	all, _ := LoadMigrations(migrations.FS)
	var count int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations`).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != len(all) {
		t.Fatalf("expected %d versions recorded, got %d", len(all), count)
	}
}
