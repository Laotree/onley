package db

import (
	"database/sql"
	"fmt"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS files (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	path       TEXT    NOT NULL UNIQUE,
	name       TEXT    NOT NULL,
	size       INTEGER NOT NULL,
	md5        TEXT    NOT NULL,
	mtime      INTEGER NOT NULL DEFAULT 0,
	scanned_at DATETIME NOT NULL DEFAULT (datetime('now'))
);
CREATE INDEX IF NOT EXISTS idx_files_md5 ON files(md5);
`

// migration adds mtime to databases created before this field existed.
// ALTER TABLE silently fails if the column is already present.
const migration = `ALTER TABLE files ADD COLUMN mtime INTEGER NOT NULL DEFAULT 0`

type FileRecord struct {
	ID    int64
	Path  string
	Name  string
	Size  int64
	MD5   string
	Mtime int64 // Unix timestamp (seconds)
}

type DB struct {
	conn *sql.DB
}

func Open(path string) (*DB, error) {
	conn, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	// Single connection: SQLite doesn't benefit from a pool, and in-memory
	// databases require a single connection so all goroutines share the same db.
	conn.SetMaxOpenConns(1)
	if _, err := conn.Exec(schema); err != nil {
		conn.Close()
		return nil, fmt.Errorf("init schema: %w", err)
	}
	conn.Exec("PRAGMA journal_mode=WAL")
	conn.Exec("PRAGMA busy_timeout=5000")
	// Best-effort migration; ignore error when column already exists.
	conn.Exec(migration)
	return &DB{conn: conn}, nil
}

func (d *DB) Close() error {
	return d.conn.Close()
}

func (d *DB) Upsert(r FileRecord) error {
	_, err := d.conn.Exec(
		`INSERT INTO files(path, name, size, md5, mtime) VALUES(?,?,?,?,?)
		 ON CONFLICT(path) DO UPDATE SET
		   name=excluded.name, size=excluded.size, md5=excluded.md5,
		   mtime=excluded.mtime, scanned_at=datetime('now')`,
		r.Path, r.Name, r.Size, r.MD5, r.Mtime,
	)
	return err
}

// Lookup returns the stored record for path, or nil if not found.
func (d *DB) Lookup(path string) (*FileRecord, error) {
	var r FileRecord
	err := d.conn.QueryRow(
		`SELECT id, path, name, size, md5, mtime FROM files WHERE path = ?`, path,
	).Scan(&r.ID, &r.Path, &r.Name, &r.Size, &r.MD5, &r.Mtime)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// DuplicateGroup holds one MD5 hash and all files that share it.
type DuplicateGroup struct {
	MD5   string
	Size  int64
	Files []FileRecord
}

func (d *DB) Duplicates() ([]DuplicateGroup, error) {
	rows, err := d.conn.Query(
		`SELECT id, path, name, size, md5, mtime FROM files
		 WHERE md5 IN (SELECT md5 FROM files GROUP BY md5 HAVING count(*) > 1)
		 ORDER BY md5, path`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	groups := map[string]*DuplicateGroup{}
	var order []string

	for rows.Next() {
		var r FileRecord
		if err := rows.Scan(&r.ID, &r.Path, &r.Name, &r.Size, &r.MD5, &r.Mtime); err != nil {
			return nil, err
		}
		if _, exists := groups[r.MD5]; !exists {
			groups[r.MD5] = &DuplicateGroup{MD5: r.MD5, Size: r.Size}
			order = append(order, r.MD5)
		}
		groups[r.MD5].Files = append(groups[r.MD5].Files, r)
	}

	result := make([]DuplicateGroup, 0, len(order))
	for _, md5 := range order {
		result = append(result, *groups[md5])
	}
	return result, rows.Err()
}

func (d *DB) DeleteRecord(path string) error {
	_, err := d.conn.Exec(`DELETE FROM files WHERE path = ?`, path)
	return err
}

// AllFiles returns every indexed file ordered by path.
func (d *DB) AllFiles() ([]FileRecord, error) {
	rows, err := d.conn.Query(
		`SELECT id, path, name, size, md5, mtime FROM files ORDER BY path`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var records []FileRecord
	for rows.Next() {
		var r FileRecord
		if err := rows.Scan(&r.ID, &r.Path, &r.Name, &r.Size, &r.MD5, &r.Mtime); err != nil {
			return nil, err
		}
		records = append(records, r)
	}
	return records, rows.Err()
}

// FindByMD5 returns all files whose MD5 matches the given hash.
func (d *DB) FindByMD5(md5 string) ([]FileRecord, error) {
	rows, err := d.conn.Query(
		`SELECT id, path, name, size, md5, mtime FROM files WHERE md5 = ?`, md5,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var records []FileRecord
	for rows.Next() {
		var r FileRecord
		if err := rows.Scan(&r.ID, &r.Path, &r.Name, &r.Size, &r.MD5, &r.Mtime); err != nil {
			return nil, err
		}
		records = append(records, r)
	}
	return records, rows.Err()
}

func (d *DB) Stats() (total int, duplicates int, err error) {
	err = d.conn.QueryRow(`SELECT COUNT(*) FROM files`).Scan(&total)
	if err != nil {
		return
	}
	err = d.conn.QueryRow(
		`SELECT COUNT(*) FROM files WHERE md5 IN (SELECT md5 FROM files GROUP BY md5 HAVING count(*) > 1)`,
	).Scan(&duplicates)
	return
}
