package web

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite" // 纯 Go 驱动，镜像保持 CGO_ENABLED=0
)

// db 保存网页端自有的数据：书库目录与每本书的对话会话。
// 作品本身（大纲、章节、进度）仍由各书目录里的 store 管理，这里只记"有哪些书、在哪"。
type db struct {
	sql          *sql.DB
	addedRenamed bool // 本次启动刚补上 renamed 列，需要回填旧数据
}

type bookRow struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Dir       string `json:"dir"`
	Renamed   bool   `json:"renamed"`
	Archived  bool   `json:"archived"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

type sessionRow struct {
	ID        int64  `json:"id"`
	BookID    string `json:"book_id"`
	Title     string `json:"title"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
	Messages  int    `json:"messages"`
}

type messageRow struct {
	ID        int64  `json:"id"`
	Role      string `json:"role"` // user / assistant / steer
	Content   string `json:"content"`
	Chapter   int    `json:"chapter,omitempty"`
	CreatedAt string `json:"created_at"`
}

var errNotFound = errors.New("không tìm thấy")

const schema = `
CREATE TABLE IF NOT EXISTS books (
	id          TEXT PRIMARY KEY,
	name        TEXT NOT NULL,
	dir         TEXT NOT NULL UNIQUE,
	archived    INTEGER NOT NULL DEFAULT 0,
	created_at  TEXT NOT NULL,
	updated_at  TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS chat_sessions (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	book_id     TEXT NOT NULL REFERENCES books(id),
	title       TEXT NOT NULL,
	created_at  TEXT NOT NULL,
	updated_at  TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_sessions_book ON chat_sessions(book_id, updated_at);
CREATE TABLE IF NOT EXISTS chat_messages (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	session_id  INTEGER NOT NULL REFERENCES chat_sessions(id) ON DELETE CASCADE,
	role        TEXT NOT NULL,
	content     TEXT NOT NULL,
	chapter     INTEGER NOT NULL DEFAULT 0,
	created_at  TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_messages_session ON chat_messages(session_id, id);
`

func openDB(path string) (*db, error) {
	// 不开 WAL：数据库常落在 Docker 绑定挂载上，共享内存文件在那里不可靠。
	conn, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	conn.SetMaxOpenConns(1)
	if _, err := conn.Exec(schema); err != nil {
		conn.Close()
		return nil, fmt.Errorf("khởi tạo CSDL %s: %w", path, err)
	}
	d := &db{sql: conn}
	if err := d.migrate(); err != nil {
		conn.Close()
		return nil, fmt.Errorf("nâng cấp CSDL %s: %w", path, err)
	}
	return d, nil
}

// migrate 给旧库补列。renamed 标记"书库名是用户手动改的"，此时它优先于作品里 AI 定的书名。
func (d *db) migrate() error {
	has, err := d.hasColumn("books", "renamed")
	if err != nil || has {
		return err
	}
	if _, err := d.sql.Exec(`ALTER TABLE books ADD COLUMN renamed INTEGER NOT NULL DEFAULT 0`); err != nil {
		return err
	}
	d.addedRenamed = true
	return nil
}

func (d *db) hasColumn(table, column string) (bool, error) {
	rows, err := d.sql.Query(`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return false, err
		}
		if name == column {
			return true, nil
		}
	}
	return false, rows.Err()
}

func (d *db) Close() error { return d.sql.Close() }

func now() string { return time.Now().Format(time.RFC3339) }

// ---- books ----

const bookCols = `id, name, dir, renamed, archived, created_at, updated_at`

type rowScanner interface{ Scan(dest ...any) error }

func scanBook(r rowScanner) (bookRow, error) {
	var b bookRow
	err := r.Scan(&b.ID, &b.Name, &b.Dir, &b.Renamed, &b.Archived, &b.CreatedAt, &b.UpdatedAt)
	return b, err
}

// listBooks 列出书库；archived=true 时只列被隐藏的书。
func (d *db) listBooks(archived bool) ([]bookRow, error) {
	rows, err := d.sql.Query(`SELECT `+bookCols+` FROM books WHERE archived = ? ORDER BY updated_at DESC`, archived)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []bookRow
	for rows.Next() {
		b, err := scanBook(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

func (d *db) getBook(id string) (bookRow, error) {
	b, err := scanBook(d.sql.QueryRow(`SELECT `+bookCols+` FROM books WHERE id = ? AND archived = 0`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return b, errNotFound
	}
	return b, err
}

// addBook 登记一本书；同一目录重复登记时恢复（取消归档）原记录。
func (d *db) addBook(id, name, dir string) (bookRow, error) {
	t := now()
	_, err := d.sql.Exec(`INSERT INTO books (id, name, dir, created_at, updated_at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(dir) DO UPDATE SET archived = 0, updated_at = excluded.updated_at`, id, name, dir, t, t)
	if err != nil {
		return bookRow{}, err
	}
	return scanBook(d.sql.QueryRow(`SELECT `+bookCols+` FROM books WHERE dir = ?`, dir))
}

func (d *db) hasBookDir(dir string) (bool, error) {
	var n int
	err := d.sql.QueryRow(`SELECT COUNT(*) FROM books WHERE dir = ?`, dir).Scan(&n)
	return n > 0, err
}

// knownDirs 返回书库里登记过的所有目录（含已隐藏的）。
func (d *db) knownDirs() (map[string]bool, error) {
	rows, err := d.sql.Query(`SELECT dir FROM books`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var dir string
		if err := rows.Scan(&dir); err != nil {
			return nil, err
		}
		out[dir] = true
	}
	return out, rows.Err()
}

func (d *db) renameBook(id, name string) error {
	return d.execOne(`UPDATE books SET name = ?, renamed = 1, updated_at = ? WHERE id = ? AND archived = 0`, name, now(), id)
}

func (d *db) markRenamed(id string) error {
	return d.execOne(`UPDATE books SET renamed = 1 WHERE id = ?`, id)
}

func (d *db) touchBook(id string) {
	_, _ = d.sql.Exec(`UPDATE books SET updated_at = ? WHERE id = ?`, now(), id)
}

// archiveBook 只把书从书库隐藏，作品目录与对话都保留，restoreBook 可恢复。
func (d *db) archiveBook(id string) error {
	return d.execOne(`UPDATE books SET archived = 1 WHERE id = ? AND archived = 0`, id)
}

func (d *db) restoreBook(id string) error {
	return d.execOne(`UPDATE books SET archived = 0, updated_at = ? WHERE id = ? AND archived = 1`, now(), id)
}

// ---- sessions ----

func (d *db) listSessions(bookID string) ([]sessionRow, error) {
	rows, err := d.sql.Query(`SELECT s.id, s.book_id, s.title, s.created_at, s.updated_at,
		(SELECT COUNT(*) FROM chat_messages m WHERE m.session_id = s.id)
		FROM chat_sessions s WHERE s.book_id = ? ORDER BY s.updated_at DESC, s.id DESC`, bookID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []sessionRow{}
	for rows.Next() {
		var s sessionRow
		if err := rows.Scan(&s.ID, &s.BookID, &s.Title, &s.CreatedAt, &s.UpdatedAt, &s.Messages); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func (d *db) getSession(id int64) (sessionRow, error) {
	var s sessionRow
	err := d.sql.QueryRow(`SELECT id, book_id, title, created_at, updated_at FROM chat_sessions WHERE id = ?`, id).
		Scan(&s.ID, &s.BookID, &s.Title, &s.CreatedAt, &s.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return s, errNotFound
	}
	return s, err
}

func (d *db) createSession(bookID, title string) (sessionRow, error) {
	if strings.TrimSpace(title) == "" {
		title = "Phiên mới"
	}
	t := now()
	res, err := d.sql.Exec(`INSERT INTO chat_sessions (book_id, title, created_at, updated_at) VALUES (?, ?, ?, ?)`, bookID, title, t, t)
	if err != nil {
		return sessionRow{}, err
	}
	id, _ := res.LastInsertId()
	return sessionRow{ID: id, BookID: bookID, Title: title, CreatedAt: t, UpdatedAt: t}, nil
}

func (d *db) renameSession(id int64, title string) error {
	return d.execOne(`UPDATE chat_sessions SET title = ? WHERE id = ?`, title, id)
}

func (d *db) deleteSession(id int64) error {
	return d.execOne(`DELETE FROM chat_sessions WHERE id = ?`, id)
}

// ---- messages ----

func (d *db) listMessages(sessionID int64) ([]messageRow, error) {
	rows, err := d.sql.Query(`SELECT id, role, content, chapter, created_at FROM chat_messages WHERE session_id = ? ORDER BY id`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []messageRow{}
	for rows.Next() {
		var m messageRow
		if err := rows.Scan(&m.ID, &m.Role, &m.Content, &m.Chapter, &m.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (d *db) addMessage(sessionID int64, role, content string, chapter int) (messageRow, error) {
	t := now()
	res, err := d.sql.Exec(`INSERT INTO chat_messages (session_id, role, content, chapter, created_at) VALUES (?, ?, ?, ?, ?)`,
		sessionID, role, content, chapter, t)
	if err != nil {
		return messageRow{}, err
	}
	_, _ = d.sql.Exec(`UPDATE chat_sessions SET updated_at = ? WHERE id = ?`, t, sessionID)
	id, _ := res.LastInsertId()
	return messageRow{ID: id, Role: role, Content: content, Chapter: chapter, CreatedAt: t}, nil
}

func (d *db) execOne(query string, args ...any) error {
	res, err := d.sql.Exec(query, args...)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errNotFound
	}
	return nil
}
