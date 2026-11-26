package store

import "database/sql"

// DiagDBStats exposes the connection pool counters to the watcher shutdown
// diagnosis on this branch only; it is not meant for merge.
func (s *Store) DiagDBStats() sql.DBStats { return s.db.Stats() }
