package sandbox

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	_ "modernc.org/sqlite"
)

type StoredResource struct {
	ID          string          `json:"id"`
	Type        string          `json:"type"`
	ResourceKey string          `json:"resourceKey"`
	Attributes  json.RawMessage `json:"attributes"`
	State       *string         `json:"state"`
	Version     int64           `json:"version"`
	SizeBytes   int64           `json:"sizeBytes"`
}

type ResourceRecord struct {
	Type             string          `json:"type"`
	ResourceKey      string          `json:"resourceKey"`
	Attributes       json.RawMessage `json:"attributes"`
	State            *string         `json:"state"`
	CreatedAtVirtual string          `json:"createdAtVirtual"`
	UpdatedAtVirtual string          `json:"updatedAtVirtual"`
}

type Page struct {
	Items         []StoredResource `json:"items"`
	NextCursorKey *string          `json:"nextCursorKey"`
}

type StoreTotals struct {
	Count int64 `json:"count"`
	Bytes int64 `json:"bytes"`
}

var (
	ErrNotFound = errors.New("not-found")
	ErrConflict = errors.New("conflict")
)

type Store struct {
	db *sql.DB
}

const schema = `
CREATE TABLE IF NOT EXISTS sandboxes (
  id TEXT PRIMARY KEY,
  -- Starts at 1.
  next_resource_seq INTEGER NOT NULL DEFAULT 1
);
CREATE TABLE IF NOT EXISTS resources (
  id TEXT PRIMARY KEY,
  sandbox_id TEXT NOT NULL,
  type TEXT NOT NULL,
  resource_key TEXT NOT NULL,
  attributes TEXT NOT NULL,
  state TEXT,
  -- Starts at 0.
  version INTEGER NOT NULL DEFAULT 0,
  size_bytes INTEGER NOT NULL,
  created_at_virtual INTEGER NOT NULL,
  updated_at_virtual INTEGER NOT NULL,
  UNIQUE (sandbox_id, type, resource_key)
);
CREATE INDEX IF NOT EXISTS idx_resources_list ON resources (sandbox_id, type, resource_key);
`

func OpenStore(dataDir string) (*Store, error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, err
	}
	dbPath := filepath.Join(dataDir, "sandbox.db")

	if f, err := os.OpenFile(dbPath, os.O_CREATE, 0o600); err == nil {
		f.Close()
	}
	db, err := sql.Open("sqlite", dbPath+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}

	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

func OpenMemoryStore() (*Store, error) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func attributeSize(attributes json.RawMessage) int64 { return int64(len(attributes)) }

func (s *Store) EnsureSandbox(sandboxID string) error {
	_, err := s.db.Exec(`INSERT INTO sandboxes (id) VALUES (?) ON CONFLICT (id) DO NOTHING`, sandboxID)
	return err
}

func (s *Store) AllocateSeq(sandboxID string) (int64, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO sandboxes (id) VALUES (?) ON CONFLICT (id) DO NOTHING`, sandboxID); err != nil {
		return 0, err
	}
	var next int64
	if err := tx.QueryRow(`UPDATE sandboxes SET next_resource_seq = next_resource_seq + 1 WHERE id = ? RETURNING next_resource_seq`, sandboxID).Scan(&next); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return next - 1, nil
}

func (s *Store) Put(sandboxID, typ, key string, attributes json.RawMessage, state *string, virtualNow int64) (int64, error) {
	size := attributeSize(attributes)
	_, err := s.db.Exec(`
INSERT INTO resources (id, sandbox_id, type, resource_key, attributes, state, size_bytes, created_at_virtual, updated_at_virtual)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (sandbox_id, type, resource_key) DO UPDATE SET
  attributes = excluded.attributes,
  state = excluded.state,
  size_bytes = excluded.size_bytes,
  updated_at_virtual = excluded.updated_at_virtual`,
		newResourceID(), sandboxID, typ, key, string(attributes), state, size, virtualNow, virtualNow)
	return size, err
}

func (s *Store) Insert(sandboxID, typ, key string, attributes json.RawMessage, state *string, virtualNow int64) (*StoredResource, error) {
	id := newResourceID()
	size := attributeSize(attributes)
	_, err := s.db.Exec(`
INSERT INTO resources (id, sandbox_id, type, resource_key, attributes, state, size_bytes, created_at_virtual, updated_at_virtual)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, sandboxID, typ, key, string(attributes), state, size, virtualNow, virtualNow)
	if err != nil {
		return nil, err
	}
	return &StoredResource{ID: id, Type: typ, ResourceKey: key, Attributes: attributes, State: state, Version: 0, SizeBytes: size}, nil
}

func (s *Store) GetOne(sandboxID, typ, key string) (*StoredResource, error) {
	row := s.db.QueryRow(`SELECT id, type, resource_key, attributes, state, version, size_bytes FROM resources WHERE sandbox_id = ? AND type = ? AND resource_key = ?`, sandboxID, typ, key)
	return scanResource(row)
}

func (s *Store) List(sandboxID, typ string, limit int, cursorKey *string) (*Page, error) {
	if limit <= 0 {
		limit = 20
	}
	after := ""
	if cursorKey != nil {
		after = *cursorKey
	}
	rows, err := s.db.Query(`
SELECT id, type, resource_key, attributes, state, version, size_bytes
FROM resources WHERE sandbox_id = ? AND type = ? AND resource_key > ?
ORDER BY resource_key ASC LIMIT ?`, sandboxID, typ, after, limit+1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var items []StoredResource
	for rows.Next() {
		r, err := scanResourceRows(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, *r)
	}
	page := &Page{Items: items}
	if len(items) > limit {
		page.Items = items[:limit]
		last := page.Items[limit-1].ResourceKey
		page.NextCursorKey = &last
	}
	return page, rows.Err()
}

func (s *Store) Update(sandboxID, typ, key string, attributes json.RawMessage, expectedVersion *int64, state *string, stateSet bool, virtualNow int64) (*StoredResource, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var id string
	var version int64
	err = tx.QueryRow(`SELECT id, version FROM resources WHERE sandbox_id = ? AND type = ? AND resource_key = ?`, sandboxID, typ, key).Scan(&id, &version)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if expectedVersion != nil && version != *expectedVersion {
		return nil, ErrConflict
	}
	if stateSet {
		_, err = tx.Exec(`UPDATE resources SET attributes = ?, size_bytes = ?, version = version + 1, state = ?, updated_at_virtual = ? WHERE id = ?`,
			string(attributes), attributeSize(attributes), state, virtualNow, id)
	} else {
		_, err = tx.Exec(`UPDATE resources SET attributes = ?, size_bytes = ?, version = version + 1, updated_at_virtual = ? WHERE id = ?`,
			string(attributes), attributeSize(attributes), virtualNow, id)
	}
	if err != nil {
		return nil, err
	}

	updated, err := scanResource(tx.QueryRow(`SELECT id, type, resource_key, attributes, state, version, size_bytes FROM resources WHERE sandbox_id = ? AND type = ? AND resource_key = ?`, sandboxID, typ, key))
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return updated, nil
}

func (s *Store) Remove(sandboxID, typ, key string) (bool, error) {
	res, err := s.db.Exec(`DELETE FROM resources WHERE sandbox_id = ? AND type = ? AND resource_key = ?`, sandboxID, typ, key)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

func (s *Store) Totals(sandboxID string) (*StoreTotals, error) {
	t := &StoreTotals{}
	err := s.db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(size_bytes), 0) FROM resources WHERE sandbox_id = ?`, sandboxID).Scan(&t.Count, &t.Bytes)
	return t, err
}

func (s *Store) Serialize(sandboxID string) ([]ResourceRecord, error) {
	rows, err := s.db.Query(`SELECT type, resource_key, attributes, state, created_at_virtual, updated_at_virtual FROM resources WHERE sandbox_id = ? ORDER BY type ASC, resource_key ASC`, sandboxID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ResourceRecord{}
	for rows.Next() {
		var r ResourceRecord
		var attrs string
		var created, updated int64
		if err := rows.Scan(&r.Type, &r.ResourceKey, &attrs, &r.State, &created, &updated); err != nil {
			return nil, err
		}
		r.Attributes = json.RawMessage(attrs)
		r.CreatedAtVirtual = fmt.Sprint(created)
		r.UpdatedAtVirtual = fmt.Sprint(updated)
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) Clear(sandboxID string) error {
	_, err := s.db.Exec(`DELETE FROM resources WHERE sandbox_id = ?`, sandboxID)
	return err
}

func (s *Store) Load(sandboxID string, records []ResourceRecord) (*StoreTotals, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	totals := &StoreTotals{}
	for _, r := range records {
		size := attributeSize(r.Attributes)
		totals.Count++
		totals.Bytes += size

		created, cerr := strconv.ParseInt(r.CreatedAtVirtual, 10, 64)
		updated, uerr := strconv.ParseInt(r.UpdatedAtVirtual, 10, 64)
		if cerr != nil || uerr != nil {
			return nil, fmt.Errorf("resource %s/%s has corrupt virtual timestamps (%q, %q)", r.Type, r.ResourceKey, r.CreatedAtVirtual, r.UpdatedAtVirtual)
		}
		if _, err := tx.Exec(`
INSERT INTO resources (id, sandbox_id, type, resource_key, attributes, state, size_bytes, created_at_virtual, updated_at_virtual)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			newResourceID(), sandboxID, r.Type, r.ResourceKey, string(r.Attributes), r.State, size, created, updated); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return totals, nil
}

type rowScanner interface{ Scan(dest ...any) error }

func scanResource(row rowScanner) (*StoredResource, error) {
	var r StoredResource
	var attrs string
	err := row.Scan(&r.ID, &r.Type, &r.ResourceKey, &attrs, &r.State, &r.Version, &r.SizeBytes)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	r.Attributes = json.RawMessage(attrs)
	return &r, nil
}

func scanResourceRows(rows *sql.Rows) (*StoredResource, error) { return scanResource(rows) }
