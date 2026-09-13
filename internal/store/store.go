// Package store persists documents/chunks/vectors in a single pure-Go SQLite
// file and serves hybrid keyword+vector retrieval.
package store

import (
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"

	_ "modernc.org/sqlite"
)

// DB wraps the sqlite file with an in-memory vector cache.
type DB struct {
	sqlDB *sql.DB
	path  string

	vecMu     sync.RWMutex
	vecCache  map[int64][]float32
	vecModel  string
	vecDim    int
	vecLoaded bool
}

const schemaVersion = 1

// Open opens (creating if needed) the database at path.
func Open(path string) (*DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	sdb, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	db := &DB{sqlDB: sdb, path: path, vecCache: map[int64][]float32{}}
	if err := db.migrate(); err != nil {
		sdb.Close()
		return nil, err
	}
	return db, nil
}

func (db *DB) Close() error { return db.sqlDB.Close() }

func (db *DB) migrate() error {
	var v int
	if err := db.sqlDB.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		return err
	}
	if v >= schemaVersion {
		return nil
	}
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS docs (
			id INTEGER PRIMARY KEY,
			url TEXT NOT NULL UNIQUE,
			final_url TEXT NOT NULL DEFAULT '',
			doc_type TEXT NOT NULL DEFAULT 'html',
			sha256 TEXT NOT NULL DEFAULT '',
			etag TEXT NOT NULL DEFAULT '',
			title TEXT NOT NULL DEFAULT '',
			extraction TEXT NOT NULL DEFAULT '',
			warning TEXT NOT NULL DEFAULT '',
			meta_json TEXT NOT NULL DEFAULT '{}',
			fetched_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE TABLE IF NOT EXISTS chunks (
			id INTEGER PRIMARY KEY,
			doc_id INTEGER NOT NULL REFERENCES docs(id) ON DELETE CASCADE,
			seq INTEGER NOT NULL,
			text TEXT NOT NULL,
			start_byte INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE INDEX IF NOT EXISTS idx_chunks_doc ON chunks(doc_id)`,
		`CREATE VIRTUAL TABLE IF NOT EXISTS chunks_fts USING fts5(
			text,
			content='chunks',
			content_rowid='id',
			tokenize='porter unicode61'
		)`,
		`CREATE TRIGGER IF NOT EXISTS chunks_ai AFTER INSERT ON chunks BEGIN
			INSERT INTO chunks_fts(rowid, text) VALUES (new.id, new.text);
		END`,
		`CREATE TRIGGER IF NOT EXISTS chunks_ad AFTER DELETE ON chunks BEGIN
			INSERT INTO chunks_fts(chunks_fts, rowid, text) VALUES('delete', old.id, old.text);
		END`,
		`CREATE TRIGGER IF NOT EXISTS chunks_au AFTER UPDATE ON chunks BEGIN
			INSERT INTO chunks_fts(chunks_fts, rowid, text) VALUES('delete', old.id, old.text);
			INSERT INTO chunks_fts(rowid, text) VALUES (new.id, new.text);
		END`,
		`CREATE TABLE IF NOT EXISTS vectors (
			chunk_id INTEGER PRIMARY KEY REFERENCES chunks(id) ON DELETE CASCADE,
			model TEXT NOT NULL,
			dim INTEGER NOT NULL,
			vec BLOB NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
		`PRAGMA user_version = 1`,
	}
	for _, s := range stmts {
		if _, err := db.sqlDB.Exec(s); err != nil {
			return fmt.Errorf("migrate: %w", err)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// documents & chunks
// ---------------------------------------------------------------------------

// DocInput describes a document up for storage.
type DocInput struct {
	URL        string
	FinalURL   string
	DocType    string
	SHA        string
	Etag       string
	Title      string
	Extraction string
	Warning    string
	MetaJSON   string
}

// UpsertDoc inserts or updates a document row. changed=false means the content
// hash is identical (the Graft-style incremental no-op).
func (db *DB) UpsertDoc(in DocInput) (docID int64, changed bool, err error) {
	var cur sql.NullString
	var id int64
	err = db.sqlDB.QueryRow(`SELECT id, sha256 FROM docs WHERE url = ?`, in.URL).Scan(&id, &cur)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		res, ierr := db.sqlDB.Exec(
			`INSERT INTO docs(url, final_url, doc_type, sha256, etag, title, extraction, warning, meta_json)
			 VALUES(?,?,?,?,?,?,?,?,?)`,
			in.URL, in.FinalURL, in.DocType, in.SHA, in.Etag, in.Title, in.Extraction, in.Warning, orEmpty(in.MetaJSON, "{}"))
		if ierr != nil {
			return 0, false, ierr
		}
		id, _ = res.LastInsertId()
		return id, true, nil
	case err != nil:
		return 0, false, err
	}
	if cur.String == in.SHA {
		// content identical: refresh bookkeeping only
		_, _ = db.sqlDB.Exec(`UPDATE docs SET fetched_at=CURRENT_TIMESTAMP, etag=?, title=?, extraction=?, warning=? WHERE id=?`,
			in.Etag, in.Title, in.Extraction, in.Warning, id)
		return id, false, nil
	}
	_, err = db.sqlDB.Exec(
		`UPDATE docs SET final_url=?, doc_type=?, sha256=?, etag=?, title=?, extraction=?, warning=?, meta_json=?, updated_at=CURRENT_TIMESTAMP, fetched_at=CURRENT_TIMESTAMP WHERE id=?`,
		in.FinalURL, in.DocType, in.SHA, in.Etag, in.Title, in.Extraction, in.Warning, orEmpty(in.MetaJSON, "{}"), id)
	if err != nil {
		return 0, false, err
	}
	// invalidate chunks (+vectors via FK cascade) — caller re-adds them
	if _, err := db.sqlDB.Exec(`DELETE FROM chunks WHERE doc_id = ?`, id); err != nil {
		return 0, false, err
	}
	db.invalidateVecCache()
	return id, true, nil
}

// ReplaceChunks rewrites the chunk rows of a document.
func (db *DB) ReplaceChunks(docID int64, texts []string, startBytes []int) error {
	tx, err := db.sqlDB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM chunks WHERE doc_id = ?`, docID); err != nil {
		return err
	}
	for i, t := range texts {
		sb := 0
		if i < len(startBytes) {
			sb = startBytes[i]
		}
		if _, err := tx.Exec(`INSERT INTO chunks(doc_id, seq, text, start_byte) VALUES(?,?,?,?)`,
			docID, i, t, sb); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// DocChunks returns the chunk ids of a document in order.
func (db *DB) DocChunks(docID int64) ([]int64, []string, error) {
	rows, err := db.sqlDB.Query(`SELECT id, text FROM chunks WHERE doc_id = ? ORDER BY seq`, docID)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var ids []int64
	var texts []string
	for rows.Next() {
		var id int64
		var t string
		if err := rows.Scan(&id, &t); err != nil {
			return nil, nil, err
		}
		ids = append(ids, id)
		texts = append(texts, t)
	}
	return ids, texts, rows.Err()
}

// AllChunkIDs lists every chunk id (for vector reindexing).
func (db *DB) AllChunkIDs() ([]int64, []string, error) {
	rows, err := db.sqlDB.Query(`SELECT id, text FROM chunks ORDER BY id`)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var ids []int64
	var texts []string
	for rows.Next() {
		var id int64
		var t string
		if err := rows.Scan(&id, &t); err != nil {
			return nil, nil, err
		}
		ids = append(ids, id)
		texts = append(texts, t)
	}
	return ids, texts, rows.Err()
}

// ---------------------------------------------------------------------------
// vectors
// ---------------------------------------------------------------------------

// SaveVectors stores/replaces float32 vectors for the given chunk ids.
func (db *DB) SaveVectors(model string, vecs map[int64][]float32) error {
	if len(vecs) == 0 {
		return nil
	}
	dim := 0
	for _, v := range vecs {
		dim = len(v)
		break
	}
	tx, err := db.sqlDB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for id, v := range vecs {
		blob := encodeVec(v)
		if _, err := tx.Exec(`INSERT INTO vectors(chunk_id, model, dim, vec) VALUES(?,?,?,?)
			ON CONFLICT(chunk_id) DO UPDATE SET model=excluded.model, dim=excluded.dim, vec=excluded.vec`,
			id, model, dim, blob); err != nil {
			return err
		}
	}
	if err := db.setMetaTx(tx, "vec_model", model); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	db.invalidateVecCache()
	return nil
}

// VectorMeta reports the model/dim currently stored, if any.
func (db *DB) VectorMeta() (model string, dim int, count int, err error) {
	_ = db.sqlDB.QueryRow(`SELECT COUNT(*), MIN(dim), MIN(model) FROM vectors`).Scan(&count, &dim, &model)
	if m, _ := db.GetMeta("vec_model"); m != "" {
		model = m
	}
	return model, dim, count, nil
}

func (db *DB) loadVecCacheLocked() error {
	if db.vecLoaded {
		return nil
	}
	rows, err := db.sqlDB.Query(`SELECT chunk_id, vec FROM vectors`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var blob []byte
		if err := rows.Scan(&id, &blob); err != nil {
			return err
		}
		db.vecCache[id] = decodeVec(blob)
	}
	db.vecLoaded = true
	return rows.Err()
}

// ensureVecCache loads the vector table into memory once.
func (db *DB) ensureVecCache() error {
	db.vecMu.Lock()
	defer db.vecMu.Unlock()
	return db.loadVecCacheLocked()
}

func (db *DB) invalidateVecCache() {
	db.vecMu.Lock()
	db.vecCache = map[int64][]float32{}
	db.vecLoaded = false
	db.vecMu.Unlock()
}

// ---------------------------------------------------------------------------
// meta & stats
// ---------------------------------------------------------------------------

func (db *DB) setMetaTx(tx *sql.Tx, k, v string) error {
	_, err := tx.Exec(`INSERT INTO meta(key, value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, k, v)
	return err
}

// SetMeta stores a key/value.
func (db *DB) SetMeta(k, v string) error {
	_, err := db.sqlDB.Exec(`INSERT INTO meta(key, value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, k, v)
	return err
}

// GetMeta reads a key/value.
func (db *DB) GetMeta(k string) (string, error) {
	var v string
	err := db.sqlDB.QueryRow(`SELECT value FROM meta WHERE key=?`, k).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

// Counts returns doc/chunk/vector totals for stats.
func (db *DB) Counts() (docs, chunks, vectors int, err error) {
	if err = db.sqlDB.QueryRow(`SELECT COUNT(*) FROM docs`).Scan(&docs); err != nil {
		return
	}
	if err = db.sqlDB.QueryRow(`SELECT COUNT(*) FROM chunks`).Scan(&chunks); err != nil {
		return
	}
	err = db.sqlDB.QueryRow(`SELECT COUNT(*) FROM vectors`).Scan(&vectors)
	return
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func encodeVec(v []float32) []byte {
	b := make([]byte, 4*len(v))
	for i, f := range v {
		binary.LittleEndian.PutUint32(b[i*4:], math.Float32bits(f))
	}
	return b
}

func decodeVec(b []byte) []float32 {
	v := make([]float32, len(b)/4)
	for i := range v {
		v[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[i*4:]))
	}
	return v
}

func cosine(a, b []float32) float64 {
	if len(a) == 0 || len(a) != len(b) {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / math.Sqrt(na*nb)
}

func orEmpty(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}
