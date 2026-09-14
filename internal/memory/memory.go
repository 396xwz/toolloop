package memory

import (
	"context"
	"database/sql"
	"fmt"
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
}

type SQLiteMemory struct {
	db *sql.DB
}

func NewSQLiteMemory(path string) (*SQLiteMemory, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	m := &SQLiteMemory{db: db}
	if err := m.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return m, nil
}

func (m *SQLiteMemory) migrate() error {
	_, err := m.db.Exec(`
CREATE TABLE IF NOT EXISTS memory (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  task_id TEXT NOT NULL,
  kind TEXT NOT NULL,
  content TEXT NOT NULL,
  created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_memory_kind ON memory(kind);
CREATE INDEX IF NOT EXISTS idx_memory_created ON memory(created_at);
`)
	return err
}

func (m *SQLiteMemory) Save(ctx context.Context, taskID, kind, content string) error {
	_, err := m.db.ExecContext(ctx,
		`INSERT INTO memory(task_id, kind, content, created_at) VALUES(?,?,?,?)`,
		taskID, kind, content, time.Now().UTC().Format(time.RFC3339),
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

func (m *SQLiteMemory) Search(ctx context.Context, query string, limit int) ([]MemoryItem, error) {
	q := "%" + query + "%"
	rows, err := m.db.QueryContext(ctx,
		`SELECT id, task_id, kind, content, created_at FROM memory
		 WHERE content LIKE ? ORDER BY id DESC LIMIT ?`, q, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanMemoryRows(rows)
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
type InMemoryMemory struct {
	items []MemoryItem
}

func (m *InMemoryMemory) Save(ctx context.Context, taskID, kind, content string) error {
	m.items = append(m.items, MemoryItem{
		ID: int64(len(m.items) + 1), TaskID: taskID, Kind: kind,
		Content: content, CreatedAt: time.Now(),
	})
	return nil
}
func (m *InMemoryMemory) Recent(ctx context.Context, limit int) ([]MemoryItem, error) {
	if limit > len(m.items) {
		limit = len(m.items)
	}
	start := len(m.items) - limit
	if start < 0 {
		start = 0
	}
	return append([]MemoryItem{}, m.items[start:]...), nil
}
func (m *InMemoryMemory) Search(ctx context.Context, query string, limit int) ([]MemoryItem, error) {
	return m.Recent(ctx, limit)
}
func (m *InMemoryMemory) Close() error { return nil }

var _ Memory = (*SQLiteMemory)(nil)
var _ Memory = (*InMemoryMemory)(nil)

// silence unused import if fmt only used in errors elsewhere
var _ = fmt.Sprintf
