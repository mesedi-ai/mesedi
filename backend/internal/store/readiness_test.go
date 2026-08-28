// Tests for readiness schema counting.
//
// These run against the REAL embedded filesystems on purpose. A test
// using a fixture FS would pass forever while the actual embed
// directive was broken or pointed at an empty directory, which is the
// same class of silent-pass bug the readiness endpoint exists to catch.

package store

import "testing"

func TestBehindIsOneSided(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		embedded int
		applied  int
		want     bool
	}{
		{"in sync", 56, 56, false},
		{"database one migration behind the binary", 56, 55, true},
		{"database far behind", 56, 0, true},
		// Applied > Embedded happens on every rolling deploy: the new
		// machine applies N+1 while the old one still serves. Calling
		// that unhealthy would drain working machines mid-deploy.
		{"database ahead of the binary during a rolling deploy", 56, 57, false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := SchemaStatus{Embedded: tc.embedded, Applied: tc.applied}.Behind()
			if got != tc.want {
				t.Errorf("Behind() = %v, want %v (embedded=%d applied=%d)",
					got, tc.want, tc.embedded, tc.applied)
			}
		})
	}
}

func TestBothEnginesEmbedMigrationsAndCountsMatch(t *testing.T) {
	t.Parallel()

	pg, err := countEmbeddedMigrations(migrationsPostgresFS, "migrations-postgres")
	if err != nil {
		t.Fatalf("count postgres migrations: %v", err)
	}
	lite, err := countEmbeddedMigrations(migrationsFS, "migrations")
	if err != nil {
		t.Fatalf("count sqlite migrations: %v", err)
	}

	if pg == 0 || lite == 0 {
		t.Fatalf("embedded count is zero (postgres=%d sqlite=%d); readiness would "+
			"never report a behind schema and would become another check that "+
			"cannot fail", pg, lite)
	}

	// The two dialects are written in lockstep and share the
	// schema_migrations version counter. A mismatch means one engine
	// received a migration the other did not, which is exactly how the
	// migration 056 outage happened: a fix applied to one store's
	// hand-written SQL and not the other's.
	if pg != lite {
		t.Errorf("migration counts differ: migrations-postgres has %d, migrations has %d. "+
			"Every migration needs both dialects.", pg, lite)
	}
}

func TestCountEmbeddedMigrationsFailsOnAnEmptyDirectory(t *testing.T) {
	t.Parallel()

	// "." inside an embed.FS holds directories, not .sql files, so this
	// exercises the zero-count guard without needing a fixture.
	if _, err := countEmbeddedMigrations(migrationsPostgresFS, "."); err == nil {
		t.Error("counting a directory with no .sql files returned no error; a silent " +
			"zero would disable the schema check entirely")
	}
}
