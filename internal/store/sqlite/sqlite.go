// Package sqlite implements store.Store on SQLite using the pure-Go
// modernc.org/sqlite driver (no CGO).
package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/mthamil107/skills-gateway/internal/bundle"
	"github.com/mthamil107/skills-gateway/internal/store"
)

const schema = `
CREATE TABLE IF NOT EXISTS skill_versions (
  namespace    TEXT NOT NULL,
  name         TEXT NOT NULL,
  version      TEXT NOT NULL,
  status       TEXT NOT NULL,
  digest       TEXT NOT NULL,
  description  TEXT NOT NULL,
  license      TEXT NOT NULL DEFAULT '',
  tags         TEXT NOT NULL DEFAULT '[]',
  metadata     TEXT NOT NULL DEFAULT '{}',
  publisher    TEXT NOT NULL,
  published_at TEXT NOT NULL,
  PRIMARY KEY (namespace, name, version)
);
CREATE TABLE IF NOT EXISTS skill_files (
  namespace TEXT NOT NULL,
  name      TEXT NOT NULL,
  version   TEXT NOT NULL,
  path      TEXT NOT NULL,
  sha256    TEXT NOT NULL,
  size      INTEGER NOT NULL,
  content   BLOB NOT NULL,
  PRIMARY KEY (namespace, name, version, path),
  FOREIGN KEY (namespace, name, version) REFERENCES skill_versions(namespace, name, version)
);
CREATE TABLE IF NOT EXISTS audit (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  time        TEXT NOT NULL,
  request_id  TEXT NOT NULL DEFAULT '',
  subject     TEXT NOT NULL DEFAULT '',
  agent_type  TEXT NOT NULL DEFAULT '',
  action      TEXT NOT NULL,
  namespace   TEXT NOT NULL DEFAULT '',
  name        TEXT NOT NULL DEFAULT '',
  version     TEXT NOT NULL DEFAULT '',
  outcome     TEXT NOT NULL,
  detail      TEXT NOT NULL DEFAULT '',
  remote_addr TEXT NOT NULL DEFAULT ''
);
-- The audit log is append-only: the database refuses edits and deletes.
CREATE TRIGGER IF NOT EXISTS audit_no_update BEFORE UPDATE ON audit
BEGIN SELECT RAISE(ABORT, 'audit log is append-only'); END;
CREATE TRIGGER IF NOT EXISTS audit_no_delete BEFORE DELETE ON audit
BEGIN SELECT RAISE(ABORT, 'audit log is append-only'); END;
-- Published content is immutable: only the status column may change.
CREATE TRIGGER IF NOT EXISTS files_no_update BEFORE UPDATE ON skill_files
BEGIN SELECT RAISE(ABORT, 'skill files are immutable'); END;
CREATE TRIGGER IF NOT EXISTS versions_immutable BEFORE UPDATE OF digest, description, license, tags, metadata, publisher, published_at ON skill_versions
BEGIN SELECT RAISE(ABORT, 'skill versions are immutable'); END;
`

type querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Store is a SQLite-backed store.Store.
type Store struct {
	db *sql.DB
	q  querier
}

// Open opens (creating if needed) the database at path.
func Open(path string) (*Store, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // SQLite has one writer; serialising avoids SQLITE_BUSY.
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &Store{db: db, q: db}, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// Tx runs fn in a transaction.
func (s *Store) Tx(ctx context.Context, fn func(store.Store) error) error {
	if _, inTx := s.q.(*sql.Tx); inTx {
		return fn(s)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(&Store{db: s.db, q: tx}); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

// tsLayout is fixed-width so timestamps sort lexically in SQL.
const tsLayout = "2006-01-02T15:04:05.000000000Z"

func ts(t time.Time) string { return t.UTC().Format(tsLayout) }

// PutVersion implements store.Store.
func (s *Store) PutVersion(ctx context.Context, v store.Version, files []bundle.File) error {
	return s.Tx(ctx, func(st store.Store) error {
		q := st.(*Store).q
		tags, _ := json.Marshal(nonNil(v.Tags))
		meta, _ := json.Marshal(v.Metadata)
		_, err := q.ExecContext(ctx, `INSERT INTO skill_versions
			(namespace,name,version,status,digest,description,license,tags,metadata,publisher,published_at)
			VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
			v.Namespace, v.Name, v.Version, string(v.Status), v.Digest, v.Description, v.License,
			string(tags), string(meta), v.Publisher, ts(v.PublishedAt))
		if err != nil {
			if strings.Contains(err.Error(), "UNIQUE constraint failed") || strings.Contains(err.Error(), "PRIMARY KEY") {
				return store.ErrVersionExists
			}
			return err
		}
		for _, f := range files {
			if _, err := q.ExecContext(ctx, `INSERT INTO skill_files
				(namespace,name,version,path,sha256,size,content) VALUES (?,?,?,?,?,?,?)`,
				v.Namespace, v.Name, v.Version, f.Path, f.SHA256, f.Size, f.Content); err != nil {
				return err
			}
		}
		return nil
	})
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

const versionCols = `namespace,name,version,status,digest,description,license,tags,metadata,publisher,published_at`

func scanVersion(sc interface{ Scan(...any) error }) (store.Version, error) {
	var v store.Version
	var status, tags, meta, pub string
	if err := sc.Scan(&v.Namespace, &v.Name, &v.Version, &status, &v.Digest, &v.Description,
		&v.License, &tags, &meta, &v.Publisher, &pub); err != nil {
		return v, err
	}
	v.Status = store.Status(status)
	_ = json.Unmarshal([]byte(tags), &v.Tags)
	_ = json.Unmarshal([]byte(meta), &v.Metadata)
	v.PublishedAt, _ = time.Parse(tsLayout, pub)
	return v, nil
}

// GetVersion implements store.Store; it includes the file manifest.
func (s *Store) GetVersion(ctx context.Context, ns, name, version string) (store.Version, error) {
	row := s.q.QueryRowContext(ctx, `SELECT `+versionCols+` FROM skill_versions
		WHERE namespace=? AND name=? AND version=?`, ns, name, version)
	v, err := scanVersion(row)
	if errors.Is(err, sql.ErrNoRows) {
		return v, store.ErrNotFound
	}
	if err != nil {
		return v, err
	}
	rows, err := s.q.QueryContext(ctx, `SELECT path,sha256,size FROM skill_files
		WHERE namespace=? AND name=? AND version=? ORDER BY path`, ns, name, version)
	if err != nil {
		return v, err
	}
	defer rows.Close()
	for rows.Next() {
		var f bundle.File
		if err := rows.Scan(&f.Path, &f.SHA256, &f.Size); err != nil {
			return v, err
		}
		v.Files = append(v.Files, f)
	}
	return v, rows.Err()
}

func (s *Store) queryVersions(ctx context.Context, where string, args ...any) ([]store.Version, error) {
	rows, err := s.q.QueryContext(ctx, `SELECT `+versionCols+` FROM skill_versions `+where+
		` ORDER BY namespace, name, published_at`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.Version
	for rows.Next() {
		v, err := scanVersion(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// ListVersions implements store.Store.
func (s *Store) ListVersions(ctx context.Context, ns, name string) ([]store.Version, error) {
	return s.queryVersions(ctx, `WHERE namespace=? AND name=?`, ns, name)
}

// ListAll implements store.Store.
func (s *Store) ListAll(ctx context.Context, f store.ListFilter) ([]store.Version, error) {
	var conds []string
	var args []any
	if f.Namespace != "" {
		conds = append(conds, "namespace=?")
		args = append(args, f.Namespace)
	}
	if f.Query != "" {
		conds = append(conds, "(instr(lower(name), ?) > 0 OR instr(lower(description), ?) > 0)")
		q := strings.ToLower(f.Query)
		args = append(args, q, q)
	}
	if f.Tag != "" {
		conds = append(conds, "EXISTS (SELECT 1 FROM json_each(tags) WHERE value = ?)")
		args = append(args, f.Tag)
	}
	where := ""
	if len(conds) > 0 {
		where = "WHERE " + strings.Join(conds, " AND ")
	}
	return s.queryVersions(ctx, where, args...)
}

// FileContent implements store.Store.
func (s *Store) FileContent(ctx context.Context, ns, name, version, path string) ([]byte, error) {
	var b []byte
	err := s.q.QueryRowContext(ctx, `SELECT content FROM skill_files
		WHERE namespace=? AND name=? AND version=? AND path=?`, ns, name, version, path).Scan(&b)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	return b, err
}

// SetStatus implements store.Store.
func (s *Store) SetStatus(ctx context.Context, ns, name, version string, st store.Status) error {
	res, err := s.q.ExecContext(ctx, `UPDATE skill_versions SET status=?
		WHERE namespace=? AND name=? AND version=?`, string(st), ns, name, version)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return store.ErrNotFound
	}
	return nil
}

// AppendAudit implements store.Store.
func (s *Store) AppendAudit(ctx context.Context, e store.Event) error {
	_, err := s.q.ExecContext(ctx, `INSERT INTO audit
		(time,request_id,subject,agent_type,action,namespace,name,version,outcome,detail,remote_addr)
		VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		ts(e.Time), e.RequestID, e.Subject, e.AgentType, e.Action, e.Namespace, e.Name,
		e.Version, e.Outcome, e.Detail, e.RemoteAddr)
	return err
}

// ExportAudit writes events at or after since as JSON Lines, oldest first.
func (s *Store) ExportAudit(ctx context.Context, since time.Time, w io.Writer) error {
	rows, err := s.q.QueryContext(ctx, `SELECT id,time,request_id,subject,agent_type,action,
		namespace,name,version,outcome,detail,remote_addr FROM audit WHERE time >= ? ORDER BY id`, ts(since))
	if err != nil {
		return err
	}
	defer rows.Close()
	enc := json.NewEncoder(w)
	for rows.Next() {
		var e store.Event
		var t string
		if err := rows.Scan(&e.ID, &t, &e.RequestID, &e.Subject, &e.AgentType, &e.Action,
			&e.Namespace, &e.Name, &e.Version, &e.Outcome, &e.Detail, &e.RemoteAddr); err != nil {
			return err
		}
		e.Time, _ = time.Parse(tsLayout, t)
		if err := enc.Encode(e); err != nil {
			return err
		}
	}
	return rows.Err()
}
