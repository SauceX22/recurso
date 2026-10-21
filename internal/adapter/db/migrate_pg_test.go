package db

import (
	"database/sql"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// RunMigrations must release its connection: every Postgres-backed test calls
// it, and leaked connections exhausted max_connections mid-package.
func TestRunMigrationsReleasesItsConnection(t *testing.T) {
	dbURL := os.Getenv("TEST_DATABASE_URL")
	if dbURL == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	conn, err := sql.Open("postgres", dbURL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	// Tag the migration runs so other tests' connections don't count.
	app := "migrate-leak-" + uuid.NewString()[:8]
	sep := "?"
	if strings.Contains(dbURL, "?") {
		sep = "&"
	}
	for i := 0; i < 3; i++ {
		if err := RunMigrations(dbURL + sep + "application_name=" + app); err != nil {
			t.Fatal(err)
		}
	}
	var open int
	if err := conn.QueryRow(`SELECT count(*) FROM pg_stat_activity WHERE application_name = $1`, app).Scan(&open); err != nil {
		t.Fatal(err)
	}
	if open != 0 {
		t.Fatalf("%d connections still open after 3 migration runs: RunMigrations leaks its connection", open)
	}
}
