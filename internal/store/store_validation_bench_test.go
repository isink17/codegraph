package store

import (
	"database/sql"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// BenchmarkSQLiteValidationMemory measures live database pages, not an empty
// database padded with unused space (which VACUUM would discard). Run serially:
// go test ./internal/store -run '^$' -bench BenchmarkSQLiteValidationMemory -benchtime=1x -count=3 -benchmem
func BenchmarkSQLiteValidationMemory(b *testing.B) {
	for _, mib := range []int{10, 100, 500} {
		for _, wal := range []bool{false, true} {
			for _, readonly := range []bool{false, true} {
				b.Run(fmt.Sprintf("%dMiB/WAL=%t/ReadOnly=%t", mib, wal, readonly), func(b *testing.B) {
					path := filepath.Join(b.TempDir(), RepoDatabaseFileName)
					s, err := Open(path)
					if err != nil {
						b.Fatal(err)
					}
					if _, err := s.db.Exec(`CREATE TABLE validation_payload(value BLOB); WITH RECURSIVE n(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM n WHERE x < ?) INSERT INTO validation_payload SELECT zeroblob(1048576) FROM n`, mib); err != nil {
						b.Fatal(err)
					}
					if err := s.Close(); err != nil {
						b.Fatal(err)
					}
					if wal {
						db, err := sql.Open(sqliteDriverName, path)
						if err != nil {
							b.Fatal(err)
						}
						defer db.Close()
						if _, err := db.Exec(`PRAGMA journal_mode=WAL; PRAGMA wal_autocheckpoint=0; UPDATE schema_migrations SET applied_at='benchmark' WHERE version=1`); err != nil {
							b.Fatal(err)
						}
					}
					info, err := os.Stat(path)
					if err != nil {
						b.Fatal(err)
					}
					var maxPeak uint64
					b.ReportAllocs()
					b.ResetTimer()
					for i := 0; i < b.N; i++ {
						b.StopTimer()
						runtime.GC()
						var before runtime.MemStats
						runtime.ReadMemStats(&before)
						stop, done := make(chan struct{}), make(chan uint64)
						go func() {
							ticker := time.NewTicker(time.Millisecond)
							defer ticker.Stop()
							peak := before.HeapAlloc
							for {
								var now runtime.MemStats
								runtime.ReadMemStats(&now)
								peak = max(peak, now.HeapAlloc)
								select {
								case <-stop:
									done <- peak - before.HeapAlloc
									return
								case <-ticker.C:
								}
							}
						}()
						b.StartTimer()
						if readonly {
							var reader *Store
							reader, err = OpenReadOnly(path, OpenOptions{})
							if err == nil {
								err = reader.Close()
							}
						} else {
							err = validateExistingDatabase(path)
						}
						b.StopTimer()
						close(stop)
						maxPeak = max(maxPeak, <-done)
						if err != nil {
							b.Fatal(err)
						}
					}
					b.ReportMetric(float64(maxPeak)/(1<<20), "peak-MiB")
					b.ReportMetric(float64(info.Size())/(1<<20), "db-MiB")
					if wal {
						walInfo, err := os.Stat(path + "-wal")
						if err != nil {
							b.Fatal(err)
						}
						b.ReportMetric(float64(walInfo.Size())/1024, "wal-KiB")
					}
					// Measure retained private input/output separately from latency and
					// heap sampling. This excludes SQLite's short-lived scratch files.
					var diskBytes int64
					if wal || readonly {
						snapshot, cleanup, err := createSQLiteValidationSnapshot(path)
						if err != nil {
							b.Fatal(err)
						}
						err = filepath.WalkDir(filepath.Dir(snapshot), func(path string, entry fs.DirEntry, err error) error {
							if err != nil {
								return err
							}
							if entry.IsDir() {
								return nil
							}
							info, err := entry.Info()
							if err != nil {
								return err
							}
							diskBytes += info.Size()
							return nil
						})
						cleanupErr := cleanup()
						if err != nil {
							b.Fatal(err)
						}
						if cleanupErr != nil {
							b.Fatal(cleanupErr)
						}
					}
					b.ReportMetric(float64(diskBytes)/(1<<20), "temp-MiB")
				})
			}
		}
	}
}
