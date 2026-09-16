package memory

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type Memory interface {
	Save(ctx context.Context, taskID, kind, content string) error
	Recent(ctx context.Context, limit int) ([]MemoryItem, error)
	Search(ctx context.Context, query string, limit int) ([]MemoryItem, error)
	Close() error
}

type MemoryItem struct {
	ID        int64
	TaskID    string
	Kind      string // "task_result" | "file_summary" | "note"
	Content   string
	CreatedAt time.Time
	Score     float64 // cosine similarity set by Search; 0 for Recent
}

type SQLiteMemory struct {
	db       *sql.DB
	embedder Embedder
}

// SQLiteMemoryV2 opens (creating if needed) the SQLite memory store with
// semantic recall. migrate() upgrades pre-embedding databases in place and
// BackfillEmbeddings then fills the missing vectors; both only touch rows
// without an embedding, so every open is safe.
func SQLiteMemoryV2(path string, embedder Embedder) (*SQLiteMemory, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	m := &SQLiteMemory{db: db, embedder: embedder}
	if err := m.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := m.BackfillEmbeddings(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return m, nil
}

func (m *SQLiteMemory) migrate() error {
	if _, err := m.db.Exec(`
CREATE TABLE IF NOT EXISTS memory (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  task_id TEXT NOT NULL,
  kind TEXT NOT NULL,
  content TEXT NOT NULL,
  created_at TEXT NOT NULL,
  embedding TEXT
);
CREATE INDEX IF NOT EXISTS idx_memory_kind ON memory(kind);
CREATE INDEX IF NOT EXISTS idx_memory_created ON memory(created_at);
`); err != nil {
		return err
	}
	// Databases created before the embedding column existed: add it so
	// legacy rows survive until BackfillEmbeddings fills them.
	if _, err := m.db.Exec(`ALTER TABLE memory ADD COLUMN embedding TEXT`); err != nil {
		if !strings.Contains(err.Error(), "duplicate column") {
			return err
		}
	}
	return nil
}

func (m *SQLiteMemory) Save(ctx context.Context, taskID, kind, content string) error {
	var embJSON string
	if m.embedder != nil {
		if emb, err := m.embedder.Embed(ctx, content); err == nil {
			if b, err := json.Marshal(emb); err == nil {
				embJSON = string(b)
			}
		}
		// Embed failure: still insert, with a NULL embedding; the item is
		// excluded from semantic recall until backfilled.
	}
	_, err := m.db.ExecContext(ctx,
		`INSERT INTO memory(task_id, kind, content, created_at, embedding) VALUES(?,?,?,?,?)`,
		taskID, kind, content, time.Now().UTC().Format(time.RFC3339), embJSON,
	)
	return err
}

func (m *SQLiteMemory) Recent(ctx context.Context, limit int) ([]MemoryItem, error) {
	rows, err := m.db.QueryContext(ctx,
		`SELECT id, task_id, kind, content, created_at FROM memory ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanMemoryRows(rows)
}

// Search recalls memory items by embedding cosine similarity. Rows without
// an embedding (embed failed at Save or not yet backfilled) are skipped.
func (m *SQLiteMemory) Search(ctx context.Context, query string, limit int) ([]MemoryItem, error) {
	if limit <= 0 {
		limit = 5
	}
	if m.embedder == nil {
		return nil, fmt.Errorf("memory: no embedder configured")
	}
	qEmb, err := m.embedder.Embed(ctx, query)
	if err != nil {
		return nil, err
	}

	rows, err := m.db.QueryContext(ctx,
		`SELECT id, task_id, kind, content, created_at, COALESCE(embedding, '') FROM memory`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type scored struct {
		it    MemoryItem
		score float64
	}
	var all []scored
	for rows.Next() {
		var it MemoryItem
		var ts, embJSON string
		if err := rows.Scan(&it.ID, &it.TaskID, &it.Kind, &it.Content, &ts, &embJSON); err != nil {
			continue
		}
		if embJSON == "" {
			continue // no embedding yet; excluded from semantic recall
		}
		var emb []float32
		if err := json.Unmarshal([]byte(embJSON), &emb); err != nil || len(emb) == 0 {
			continue
		}
		it.CreatedAt, _ = time.Parse(time.RFC3339, ts)
		it.Score = cosine(qEmb, emb)
		all = append(all, scored{it: it, score: it.Score})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(all, func(i, j int) bool { return all[i].score > all[j].score })
	if len(all) > limit {
		all = all[:limit]
	}
	out := make([]MemoryItem, 0, len(all))
	for _, s := range all {
		out = append(out, s.it)
	}
	return out, nil
}

// BackfillEmbeddings embeds rows whose embedding is missing. Idempotent:
// rows that already have an embedding are left alone, and rows whose embed
// call fails stay NULL to be retried on the next open.
func (m *SQLiteMemory) BackfillEmbeddings(ctx context.Context) error {
	if m.embedder == nil {
		return nil
	}
	rows, err := m.db.QueryContext(ctx,
		`SELECT id, content FROM memory WHERE embedding IS NULL OR embedding = ''`)
	if err != nil {
		return err
	}
	type pending struct {
		id      int64
		content string
	}
	var todo []pending
	for rows.Next() {
		var p pending
		if err := rows.Scan(&p.id, &p.content); err != nil {
			rows.Close()
			return err
		}
		todo = append(todo, p)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, p := range todo {
		emb, err := m.embedder.Embed(ctx, p.content)
		if err != nil {
			continue
		}
		b, err := json.Marshal(emb)
		if err != nil {
			continue
		}
		if _, err := m.db.ExecContext(ctx, `UPDATE memory SET embedding = ? WHERE id = ?`, string(b), p.id); err != nil {
			return err
		}
	}
	return nil
}

func (m *SQLiteMemory) Close() error { return m.db.Close() }

func scanMemoryRows(rows *sql.Rows) ([]MemoryItem, error) {
	var out []MemoryItem
	for rows.Next() {
		var it MemoryItem
		var ts string
		if err := rows.Scan(&it.ID, &it.TaskID, &it.Kind, &it.Content, &ts); err != nil {
			return nil, err
		}
		it.CreatedAt, _ = time.Parse(time.RFC3339, ts)
		out = append(out, it)
	}
	return out, rows.Err()
}

// Keep a simple in-memory fallback if needed
type InMemory struct {
	items []MemoryItem
}

func (m *InMemory) Save(ctx context.Context, taskID, kind, content string) error {
	m.items = append(m.items, MemoryItem{
		ID: int64(len(m.items) + 1), TaskID: taskID, Kind: kind,
		Content: content, CreatedAt: time.Now(),
	})
	return nil
}
func (m *InMemory) Recent(ctx context.Context, limit int) ([]MemoryItem, error) {
	if limit > len(m.items) {
		limit = len(m.items)
	}
	start := len(m.items) - limit
	if start < 0 {
		start = 0
	}
	return append([]MemoryItem{}, m.items[start:]...), nil
}
func (m *InMemory) Search(ctx context.Context, query string, limit int) ([]MemoryItem, error) {
	return m.Recent(ctx, limit)
}
func (m *InMemory) Close() error { return nil }

var _ Memory = (*SQLiteMemory)(nil)
var _ Memory = (*InMemory)(nil)
