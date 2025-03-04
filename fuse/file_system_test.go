package fuse_test

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/mattn/go-sqlite3"
	"github.com/superfly/litefs"
	"github.com/superfly/litefs/fuse"
	"github.com/superfly/litefs/internal/testingutil"
	"golang.org/x/sync/errgroup"
)

func TestFileSystem_OK(t *testing.T) {
	fs := newOpenFileSystem(t, t.TempDir(), litefs.NewStaticLeaser(true, "localhost", "http://localhost:20202"))
	dsn := filepath.Join(fs.Path(), "db")
	db := testingutil.OpenSQLDB(t, dsn)

	// Create a simple table with a single value.
	if _, err := db.Exec(`CREATE TABLE t (x)`); err != nil {
		t.Fatal(err)
	}

	txID := fs.Store().DB("db").TXID()
	if _, err := db.Exec(`INSERT INTO t VALUES (100)`); err != nil {
		t.Fatal(err)
	}

	// Ensure we can retrieve the data back from the database.
	var x int
	if err := db.QueryRow(`SELECT x FROM t`).Scan(&x); err != nil {
		t.Fatal(err)
	} else if got, want := x, 100; got != want {
		t.Fatalf("x=%d, want %d", got, want)
	}

	// Verify transaction count.
	if got, want := fs.Store().DB("db").TXID(), txID+1; got != want {
		t.Fatalf("txid=%d, want %d", got, want)
	}

	// Close & reopen.
	testingutil.ReopenSQLDB(t, &db, dsn)
	db = testingutil.OpenSQLDB(t, dsn)
	if _, err := db.Exec(`INSERT INTO t VALUES (200)`); err != nil {
		t.Fatal(err)
	}

	// Ensure we can retrieve the data back from the database.
	rows, err := db.Query(`SELECT x FROM t`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()

	var values []int
	for rows.Next() {
		var x int
		if err := rows.Scan(&x); err != nil {
			t.Fatal(err)
		}
		values = append(values, x)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}

	if got, want := values, []int{100, 200}; !reflect.DeepEqual(got, want) {
		t.Fatalf("values=%d, want %d", got, want)
	}

	// Verify new transaction count.
	if got, want := fs.Store().DB("db").TXID(), txID+2; got != want {
		t.Fatalf("txid=%d, want %d", got, want)
	}
}

func TestFileSystem_Rollback(t *testing.T) {
	fs := newOpenFileSystem(t, t.TempDir(), litefs.NewStaticLeaser(true, "localhost", "http://localhost:20202"))
	dsn := filepath.Join(fs.Path(), "db")
	db := testingutil.OpenSQLDB(t, dsn)

	// Create a simple table with a single value.
	if _, err := db.Exec(`CREATE TABLE t (x)`); err != nil {
		t.Fatal(err)
	}

	// Attempt to insert data but roll it back.
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	} else if _, err := tx.Exec(`INSERT INTO t VALUES (100)`); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	} else if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}

	// Ensure we can retrieve the data back from the database.
	var x int
	if err := db.QueryRow(`SELECT x FROM t`).Scan(&x); err != sql.ErrNoRows {
		t.Fatalf("expected no rows (%#v)", err)
	}
}

func TestFileSystem_RollbackJournalOnStartup(t *testing.T) {
	fs := newOpenFileSystem(t, t.TempDir(), litefs.NewStaticLeaser(true, "localhost", "http://localhost:20202"))
	db := testingutil.OpenSQLDB(t, filepath.Join(fs.Path(), "db"))
	if _, err := db.Exec(`PRAGMA cache_size = 2`); err != nil {
		t.Fatal(err)
	} else if _, err := db.Exec(`CREATE TABLE t (x)`); err != nil {
		t.Fatal(err)
	}

	// Begin writing a transaction.
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()

	for i := 0; i < 100; i++ {
		if _, err := tx.Exec(`INSERT INTO t VALUES (?)`, strings.Repeat("x", 500)); err != nil {
			t.Fatal(err)
		}
	}

	// Copy state to another temp directory.
	tempDir := t.TempDir()
	testingutil.MustCopyDir(t, fs.Store().Path(), filepath.Join(tempDir, "data"))

	// Close initial file system.
	t.Logf("closing litefs store")
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	} else if err := db.Close(); err != nil {
		t.Fatal(err)
	} else if err := fs.Unmount(); err != nil {
		t.Fatal(err)
	}

	t.Logf("reopening litefs at: %s", tempDir)

	// Reopen file system from state copied to new path.
	fs = newOpenFileSystem(t, tempDir, litefs.NewStaticLeaser(true, "localhost", "http://localhost:20202"))
	db = testingutil.OpenSQLDB(t, filepath.Join(fs.Path(), "db"))
	if _, err := db.Exec(`PRAGMA integrity_check`); err != nil {
		t.Fatal(err)
	}

	// Ensure that no rows were written.
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM t`).Scan(&n); err != nil {
		t.Fatal(err)
	} else if got, want := n, 0; got != want {
		t.Fatalf("n=%d, want %d", got, want)
	}
}

func TestFileSystem_NoWrite(t *testing.T) {
	fs := newOpenFileSystem(t, t.TempDir(), litefs.NewStaticLeaser(true, "localhost", "http://localhost:20202"))
	dsn := filepath.Join(fs.Path(), "db")
	db := testingutil.OpenSQLDB(t, dsn)

	var txID uint64
	if db := fs.Store().DB("db"); db != nil {
		txID = db.TXID()
	}

	// Create a simple table with a single value.
	if _, err := db.Exec(`CREATE TABLE t (x)`); err != nil {
		t.Fatal(err)
	} else if got, want := fs.Store().DB("db").TXID(), txID+1; got != want {
		t.Fatalf("txid=%d, want %d", got, want)
	}

	// Start and commit a transaction without a write.
	if _, err := db.Exec(`BEGIN IMMEDIATE; COMMIT`); err != nil {
		t.Fatal(err)
	}

	// Ensure the transaction ID has not incremented.
	if got, want := fs.Store().DB("db").TXID(), txID+1; got != want {
		t.Fatalf("txid=%d, want %d", got, want)
	}
}

func TestFileSystem_MultipleJournalSegments(t *testing.T) {
	fs := newOpenFileSystem(t, t.TempDir(), litefs.NewStaticLeaser(true, "localhost", "http://localhost:20202"))
	dsn := filepath.Join(fs.Path(), "db")
	db := testingutil.OpenSQLDB(t, dsn)
	const rowN = 1000

	// Ensure cache size is low so we get multiple segments flushed.
	if _, err := db.Exec(`PRAGMA cache_size = 10`); err != nil {
		t.Fatal(err)
	}

	// Create a simple table with a single value.
	if _, err := db.Exec(`CREATE TABLE t (id INTEGER PRIMARY KEY, x TEXT)`); err != nil {
		t.Fatal(err)
	}
	txID := fs.Store().DB("db").TXID()

	// Create rows that span over many pages.
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()

	for i := 1; i <= rowN; i++ {
		if _, err := tx.Exec(`INSERT INTO t (id, x) VALUES (?, ?)`, i, strings.Repeat(fmt.Sprintf("%08x", i), 100)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	// Create a transaction with large values so it creates a lot of pages.
	tx, err = db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()

	for i := 1; i <= rowN; i++ {
		if _, err := tx.Exec(`UPDATE t SET x =? WHERE id = ?`, strings.Repeat(fmt.Sprintf("%04x", i), 100), i); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	// Ensure the transaction ID has not incremented.
	if got, want := fs.Store().DB("db").TXID(), uint64(txID+2); got != want {
		t.Fatalf("txid=%d, want %d", got, want)
	}

	// Verify database integrity
	if _, err := db.Exec(`PRAGMA integrity_check`); err != nil {
		t.Fatal(err)
	}
}

func TestFileSystem_ReadOnly(t *testing.T) {
	dir := t.TempDir()
	fs := newOpenFileSystem(t, dir, litefs.NewStaticLeaser(true, "localhost", "http://localhost:20202"))
	dsn := filepath.Join(fs.Path(), "db")

	// Create database.
	db := testingutil.OpenSQLDB(t, dsn)
	if _, err := db.Exec(`CREATE TABLE t (x)`); err != nil {
		t.Fatal(err)
	} else if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopen file system as read-only.
	if err := fs.Unmount(); err != nil {
		t.Fatal(err)
	} else if err := fs.Store().Close(); err != nil {
		t.Fatal(err)
	}

	newOpenFileSystem(t, dir, litefs.NewStaticLeaser(false, "localhost", "http://localhost:20202"))

	// Attempt to write to read-only database.
	db = testingutil.OpenSQLDB(t, dsn)
	_, err := db.Exec(`INSERT INTO t VALUES (100)`)

	switch mode := testingutil.JournalMode(); mode {
	case "delete", "persist", "truncate":
		var e sqlite3.Error
		if !errors.As(err, &e) || e.Code != sqlite3.ErrReadonly {
			t.Fatalf("unexpected error: %s", err)
		}
	case "wal":
		if err == nil || err.Error() != `disk I/O error` {
			t.Fatalf("unexpected error: %s", err)
		}
	default:
		t.Fatalf("invalid journal mode: %q", mode)
	}

	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestFileSystem_ReadDir(t *testing.T) {
	fs := newOpenFileSystem(t, t.TempDir(), litefs.NewStaticLeaser(true, "localhost", "http://localhost:20202"))
	db0 := testingutil.OpenSQLDB(t, filepath.Join(fs.Path(), "db0"))
	db1 := testingutil.OpenSQLDB(t, filepath.Join(fs.Path(), "db1"))

	if _, err := db0.Exec(`CREATE TABLE t (x)`); err != nil {
		t.Fatal(err)
	} else if _, err := db1.Exec(`CREATE TABLE t (x)`); err != nil {
		t.Fatal(err)
	}

	var want string
	switch testingutil.JournalMode() {
	case "wal":
		want = "db0\ndb0-pos\ndb0-shm\ndb0-wal\ndb1\ndb1-pos\ndb1-shm\ndb1-wal\n"
	case "persist", "truncate":
		want = "db0\ndb0-journal\ndb0-pos\ndb1\ndb1-journal\ndb1-pos\n"
	default:
		want = "db0\ndb0-pos\ndb1\ndb1-pos\n"
	}

	// Read directory listing from mount.
	cmd := exec.Command("ls")
	cmd.Dir = fs.Path()
	if buf, err := cmd.CombinedOutput(); err != nil {
		t.Fatal(err)
	} else if got := string(buf); got != want {
		t.Fatalf("unexpected output: %q", got)
	}
}

// Ensure that a partial database file doesn't prevent opening.
func TestFileSystem_ContinueOnDatabaseInitEOF(t *testing.T) {
	dir := t.TempDir()
	fs := newOpenFileSystem(t, dir, litefs.NewStaticLeaser(true, "localhost", "http://localhost:20202"))
	db := testingutil.OpenSQLDB(t, filepath.Join(fs.Path(), "db"))
	if _, err := db.Exec(`CREATE TABLE t0 (x)`); err != nil {
		t.Fatal(err)
	} else if _, err := db.Exec(`CREATE TABLE t1 (x)`); err != nil {
		t.Fatal(err)
	}

	t.Logf("closing litefs store")
	if err := db.Close(); err != nil {
		t.Fatal(err)
	} else if err := fs.Unmount(); err != nil {
		t.Fatal(err)
	}

	// Truncate one page. This page should be recovered by the last LTX.
	fi, err := os.Stat(fs.Store().DB("db").DatabasePath())
	if err != nil {
		t.Fatal(err)
	} else if err := os.Truncate(fs.Store().DB("db").DatabasePath(), fi.Size()-int64(testingutil.PageSize())); err != nil {
		t.Fatal(err)
	}

	// Reopen file system and ensure database is OK.
	t.Logf("reopening litefs")
	fs = newOpenFileSystem(t, dir, litefs.NewStaticLeaser(true, "localhost", "http://localhost:20202"))
	db = testingutil.OpenSQLDB(t, filepath.Join(fs.Path(), "db"))
	if _, err := db.Exec(`PRAGMA integrity_check`); err != nil {
		t.Fatal(err)
	}
}

// Ensures the statfs() executes and does not panic.
func TestFileSystem_Statfs(t *testing.T) {
	fs := newOpenFileSystem(t, t.TempDir(), litefs.NewStaticLeaser(true, "localhost", "http://localhost:20202"))
	var statfs syscall.Statfs_t
	if err := syscall.Statfs(fs.Path(), &statfs); err != nil {
		t.Fatal(err)
	}
}

func TestFileSystem_Pos(t *testing.T) {
	t.Run("ReopenHandle", func(t *testing.T) {
		fs := newOpenFileSystem(t, t.TempDir(), litefs.NewStaticLeaser(true, "localhost", "http://localhost:20202"))
		dsn := filepath.Join(fs.Path(), "db")
		db := testingutil.OpenSQLDB(t, dsn)

		// Write a transaction & verify the position.
		if _, err := db.Exec(`CREATE TABLE t (x)`); err != nil {
			t.Fatal(err)
		}
		if buf, err := os.ReadFile(dsn + "-pos"); err != nil {
			t.Fatal(err)
		} else if testingutil.IsWALMode() {
			if got, want := string(buf), "0000000000000002/95af056ca07d8ad1\n"; got != want {
				t.Fatalf("pos=%q, want %q", got, want)
			}
		} else {
			if got, want := string(buf), "0000000000000001/f630f5ae3060002c\n"; got != want {
				t.Fatalf("pos=%q, want %q", got, want)
			}
		}

		// Write another transaction & ensure position changes.
		if _, err := db.Exec(`INSERT INTO t VALUES (100)`); err != nil {
			t.Fatal(err)
		}
		if buf, err := os.ReadFile(dsn + "-pos"); err != nil {
			t.Fatal(err)
		} else if testingutil.IsWALMode() {
			if got, want := string(buf), "0000000000000003/f0f79787d8602c29\n"; got != want {
				t.Fatalf("pos=%q, want %q", got, want)
			}
		} else {
			if got, want := string(buf), "0000000000000002/b765dce8249e9b4b\n"; got != want {
				t.Fatalf("pos=%q, want %q", got, want)
			}
		}
	})

	t.Run("ReuseHandle", func(t *testing.T) {
		fs := newOpenFileSystem(t, t.TempDir(), litefs.NewStaticLeaser(true, "localhost", "http://localhost:20202"))
		dsn := filepath.Join(fs.Path(), "db")
		db := testingutil.OpenSQLDB(t, dsn)

		// Write a transaction,
		if _, err := db.Exec(`CREATE TABLE t (x)`); err != nil {
			t.Fatal(err)
		}

		// Open the "pos" handle once and reuse it.
		posFile, err := os.Open(dsn + "-pos")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = posFile.Close() }()

		buf := make([]byte, fuse.PosFileSize)
		if _, err := posFile.ReadAt(buf, 0); err != nil {
			t.Fatal(err)
		} else if testingutil.IsWALMode() {
			if got, want := string(buf), "0000000000000002/95af056ca07d8ad1\n"; got != want {
				t.Fatalf("pos=%q, want %q", got, want)
			}
		} else {
			if got, want := string(buf), "0000000000000001/f630f5ae3060002c\n"; got != want {
				t.Fatalf("pos=%q, want %q", got, want)
			}
		}

		// Write another transaction & ensure position changes.
		if _, err := db.Exec(`INSERT INTO t VALUES (100)`); err != nil {
			t.Fatal(err)
		}
		if _, err := posFile.ReadAt(buf, 0); err != nil {
			t.Fatal(err)
		} else if testingutil.IsWALMode() {
			if got, want := string(buf), "0000000000000003/f0f79787d8602c29\n"; got != want {
				t.Fatalf("pos=%q, want %q", got, want)
			}
		} else {
			if got, want := string(buf), "0000000000000002/b765dce8249e9b4b\n"; got != want {
				t.Fatalf("pos=%q, want %q", got, want)
			}
		}
	})
}

func TestFileSystem_MultipleTx(t *testing.T) {
	fs := newOpenFileSystem(t, t.TempDir(), litefs.NewStaticLeaser(true, "localhost", "http://localhost:20202"))
	dsn := filepath.Join(fs.Path(), "db")
	db := testingutil.OpenSQLDB(t, dsn)

	// Start with some data.
	if _, err := db.Exec(`PRAGMA busy_timeout = 2000`); err != nil {
		t.Fatal(err)
	} else if _, err := db.Exec(`CREATE TABLE t (x)`); err != nil {
		t.Fatal(err)
	} else if _, err := db.Exec(`INSERT INTO t VALUES (100)`); err != nil {
		t.Fatal(err)
	}

	// Start a read transaction.
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()

	var x int
	if err := tx.QueryRow(`SELECT x FROM t`).Scan(&x); err != nil {
		t.Fatal(err)
	}

	// Try to write at the same time.
	ch := make(chan error)
	go func() {
		_, err := db.Exec(`INSERT INTO t VALUES (200)`)
		ch <- err
	}()

	// Wait for a moment & rollback.
	time.Sleep(1 * time.Second)
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}

	// Wait for write transaction.
	if err := <-ch; err != nil {
		t.Fatal(err)
	}
}

// Ensure that LiteFS can handle a shrinking database.
func TestFileSystem_Vacuum(t *testing.T) {
	fs := newOpenFileSystem(t, t.TempDir(), litefs.NewStaticLeaser(true, "localhost", "http://localhost:20202"))
	dsn := filepath.Join(fs.Path(), "db")
	db := testingutil.OpenSQLDB(t, dsn)

	// Create a table and fill it with data.
	func() {
		tx, err := db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback() }()

		if _, err := tx.Exec(`CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT)`); err != nil {
			t.Fatal(err)
		}
		for i := 1; i <= 1000; i++ {
			if _, err := tx.Exec(`INSERT INTO t (id, v) VALUES (?, ?)`, i, strings.Repeat("x", 500)); err != nil {
				t.Fatal(err)
			}
		}

		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}()

	// Delete all data & vacuum.
	if _, err := db.Exec(`DELETE FROM t`); err != nil {
		t.Fatal(err)
	} else if _, err := db.Exec(`VACUUM`); err != nil {
		t.Fatal(err)
	}

	// Ensure we can retrieve the data back from the database.
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM t`).Scan(&n); err != nil {
		t.Fatal(err)
	} else if got, want := n, 0; got != want {
		t.Fatalf("x=%d, want %d", got, want)
	}
}

func TestFileSystem_Checkpoint(t *testing.T) {
	if !testingutil.IsWALMode() {
		t.Skip("checkpointing does not apply to the rollback journal, skipping")
	}
	for _, mode := range []string{"PASSIVE", "FULL", "RESTART", "TRUNCATE"} {
		t.Run(mode, func(t *testing.T) { testFileSystem_Checkpoint(t, mode) })
	}
}

func testFileSystem_Checkpoint(t *testing.T, mode string) {
	fs := newOpenFileSystem(t, t.TempDir(), litefs.NewStaticLeaser(true, "localhost", "http://localhost:20202"))
	dsn := filepath.Join(fs.Path(), "db")
	db := testingutil.OpenSQLDB(t, dsn)

	if _, err := db.Exec(`CREATE TABLE t (x)`); err != nil {
		t.Fatal(err)
	} else if _, err := db.Exec(`INSERT INTO t VALUES (100)`); err != nil {
		t.Fatal(err)
	}

	t.Logf("checkpointing...")
	var row [3]int
	if err := db.QueryRow(`PRAGMA wal_checkpoint(`+mode+`)`).Scan(&row[0], &row[1], &row[2]); err != nil {
		t.Fatal(err)
	}
	t.Logf("checkpoint DONE")

	t.Logf("PRAGMA wal_checkpoint(%s) => %v", mode, row)
	if row[0] != 0 {
		t.Fatal("checkpoint blocked")
	}

	if _, err := db.Exec(`INSERT INTO t VALUES (200)`); err != nil {
		t.Fatal(err)
	}

	var sum int
	if err := db.QueryRow(`SELECT SUM(x) FROM t`).Scan(&sum); err != nil {
		t.Fatal(err)
	} else if got, want := sum, 300; got != want {
		t.Fatalf("sum()=%v, want %v", got, want)
	}
}

func TestFileSystem_ConcurrentWriteAndSnapshot(t *testing.T) {
	fs := newOpenFileSystem(t, t.TempDir(), litefs.NewStaticLeaser(true, "localhost", "http://localhost:20202"))
	dsn := filepath.Join(fs.Path(), "db")
	db := testingutil.OpenSQLDB(t, dsn)

	// Start with some data.
	if _, err := db.Exec(`PRAGMA busy_timeout = 2000`); err != nil {
		t.Fatal(err)
	} else if _, err := db.Exec(`CREATE TABLE t (x)`); err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	var g errgroup.Group

	// Continuously write data in one goroutine.
	g.Go(func() error {
		defer close(done)
		for i := 0; i < 100; i++ {
			b := make([]byte, rand.Intn(256))
			rand.Read(b)
			if _, err := db.Exec(`INSERT INTO t VALUES (?)`, fmt.Sprintf("%x", b)); err != nil {
				return fmt.Errorf("insert: %w", err)
			}
		}
		return nil
	})

	// Continuously fetch a snapshot in another goroutine.
	g.Go(func() error {
		for {
			select {
			case <-done:
				return nil
			default:
				var buf bytes.Buffer
				if _, _, err := fs.Store().DB("db").WriteSnapshotTo(context.Background(), &buf); err != nil {
					return fmt.Errorf("write snapshot: %w", err)
				}
			}
		}
		return nil
	})

	// Verify no errors occurred.
	if err := g.Wait(); err != nil {
		t.Fatal(err)
	}
}

// Ensure that the last LTX file and the WAL sync positions on restart.
func TestFileSystem_OutOfSyncWAL(t *testing.T) {
	if !testingutil.IsWALMode() {
		t.Skip("does not apply to rollback journal, skipping")
	}

	dir := t.TempDir()
	fs := newOpenFileSystem(t, dir, litefs.NewStaticLeaser(true, "localhost", "http://localhost:20202"))
	db, err := sql.Open("sqlite3-persist-wal", filepath.Join(fs.Path(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	// Set WAL mode & create a simple schema.
	if _, err := db.Exec(`PRAGMA journal_mode = WAL`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE t (x)`); err != nil {
		t.Fatal(err)
	}

	execInserts := func(n int) {
		tx, err := db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback() }()

		for i := 0; i < n; i++ {
			if _, err := tx.Exec(`INSERT INTO t VALUES (?)`, strings.Repeat("x", 256)); err != nil {
				t.Fatal(err)
			}
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}

	execInserts(1)
	execInserts(10)
	execInserts(5)

	// Ensure the count is correct.
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM t`).Scan(&n); err != nil {
		t.Fatal(err)
	} else if got, want := n, 16; got != want {
		t.Fatalf("n=%d, want %d", got, want)
	}

	// Verify transaction count.
	if got, want := fs.Store().DB("db").TXID(), uint64(5); got != want {
		t.Fatalf("txid=%d, want %d", got, want)
	}

	// Read database & WAL files in case they get checkpointed by the client.
	databaseData, err := os.ReadFile(fs.Store().DB("db").DatabasePath())
	if err != nil {
		t.Fatal(err)
	}
	walData, err := os.ReadFile(fs.Store().DB("db").WALPath())
	if err != nil {
		t.Fatal(err)
	}

	// Close file system.
	if err := db.Close(); err != nil {
		t.Fatal(err)
	} else if err := fs.Store().Close(); err != nil {
		t.Fatal(err)
	} else if err := fs.Unmount(); err != nil {
		t.Fatal(err)
	}

	// Remove last LTX file to simulate an unsync'd file.
	if err := os.Remove(fs.Store().DB("db").LTXPath(5, 5)); err != nil {
		t.Fatal(err)
	}

	// Rewrite the database & WAL state.
	if err := os.WriteFile(fs.Store().DB("db").DatabasePath(), databaseData, 0666); err != nil {
		t.Fatal(err)
	} else if err := os.WriteFile(fs.Store().DB("db").WALPath(), walData, 0666); err != nil {
		t.Fatal(err)
	}

	// Reopen filesystem and ensure it recovers.
	fs = newOpenFileSystem(t, dir, litefs.NewStaticLeaser(true, "localhost", "http://localhost:20202"))
	db, err = sql.Open("sqlite3-persist-wal", filepath.Join(fs.Path(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	// Ensure the count matches the count without the last transaction.
	if err := db.QueryRow(`SELECT COUNT(*) FROM t`).Scan(&n); err != nil {
		t.Fatal(err)
	} else if got, want := n, 11; got != want {
		t.Fatalf("n=%d, want %d", got, want)
	}

	// Verify transaction count.
	if got, want := fs.Store().DB("db").TXID(), uint64(4); got != want {
		t.Fatalf("txid=%d, want %d", got, want)
	}
}

func TestFileSystem_SkipLockPage(t *testing.T) {
	if !*long {
		t.Skip("Must specify -long to run this test. It takes a while.")
	}

	dir := t.TempDir()
	fs := newOpenFileSystem(t, dir, litefs.NewStaticLeaser(true, "localhost", "http://localhost:20202"))
	dsn := filepath.Join(fs.Path(), "db")
	db := testingutil.OpenSQLDB(t, dsn)

	if _, err := db.Exec(`CREATE TABLE t (x)`); err != nil {
		t.Fatal(err)
	}

	// Write over 1GB of data.
	for i := 0; i < 11; i++ {
		t.Logf("tx %d", i)
		if _, err := db.Exec(`INSERT INTO t VALUES (?)`, make([]byte, 100*1024*1024)); err != nil {
			t.Fatal(err)
		}
	}

	// Truncate to database.
	if _, err := db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatal(err)
	}

	if fi, err := os.Stat(dsn); err != nil {
		t.Fatal(err)
	} else if fi.Size() < 1<<30 {
		t.Fatalf("database size less than 1GB: %d bytes", fi.Size())
	}

	// Import the database into itself.
	f, err := os.Open(filepath.Join(dir, "data", "dbs", "db", "database"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()

	if err := fs.Store().DB("db").Import(context.Background(), f); err != nil {
		t.Fatal(err)
	}
}

func newFileSystem(tb testing.TB, path string, leaser litefs.Leaser) *fuse.FileSystem {
	tb.Helper()

	store := litefs.NewStore(filepath.Join(path, "data"), true)
	store.StrictVerify = true
	store.Compress = testingutil.Compress()
	store.Leaser = leaser
	if err := store.Open(); err != nil {
		tb.Fatalf("cannot open store: %s", err)
	}

	fs := fuse.NewFileSystem(filepath.Join(path, "mnt"), store)
	fs.Debug = *fuseDebug

	store.Invalidator = fs

	return fs
}

func newOpenFileSystem(tb testing.TB, path string, leaser litefs.Leaser) *fuse.FileSystem {
	tb.Helper()

	fs := newFileSystem(tb, path, leaser)
	if err := fs.Mount(); err != nil {
		tb.Fatalf("cannot open file system: %s", err)
	}

	tb.Cleanup(func() {
		if err := fs.Unmount(); err != nil {
			tb.Errorf("server close failed: %s", err)
		} else if err := os.RemoveAll(fs.Path()); err != nil {
			tb.Errorf("mount path removal failed: %s", err)
		} else if err := os.RemoveAll(fs.Store().Path()); err != nil {
			tb.Errorf("data path removal failed: %s", err)
		}
	})

	return fs
}
