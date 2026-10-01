//go:build !windows

package store

func isTransientSQLiteInspectionLock(err error) bool {
	return false
}
