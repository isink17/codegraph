package store

// SQLiteDriverName reports the registered database/sql driver name used by this build.
// This is useful for benchmarks comparing the pure-Go and CGO-backed driver variants.
func SQLiteDriverName() string {
	return sqliteDriverName
}
