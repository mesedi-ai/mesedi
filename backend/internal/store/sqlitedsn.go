package store

// DSN normalization for the SQLite store, task #57.
//
// Without _time_format, the modernc driver stores a Go time.Time as
// Go's own Time.String() text ("2026-09-09 12:45:11.06 +0000 UTC").
// SQLite's date()/datetime()/strftime() cannot parse that suffix, so
// day-grouping returned NULL days, and any query comparing such a
// column against an RFC3339 string matched nothing (or, for a
// less-than cutoff, everything: retention would have purged every
// row). With _time_format=sqlite the driver writes
// "2026-09-09 12:45:11.06+00:00", which SQLite's date functions parse
// and which orders correctly both against time.Time bounds and, at
// second granularity, against legacy-format rows already on disk in
// pre-#57 development databases.
//
// The companion rule lives at every query site: bounds are bound as
// time.Time values, never as Format(...) strings, matching the
// Postgres twin methods. Probed empirically on 2026-09-09 before this
// file existed; see sqlite_sum_execution_cost_test.go for the test
// that first caught the family.

import "strings"

// withSQLiteTimeFormat returns the DSN with _time_format=sqlite
// appended unless the caller already chose a time format explicitly.
func withSQLiteTimeFormat(dsn string) string {
	if strings.Contains(dsn, "_time_format=") {
		return dsn
	}
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	return dsn + sep + "_time_format=sqlite"
}
