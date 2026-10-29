package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

const (
	postPending = "pending"
	postSent    = "sent"
	postSkipped = "skipped"

	deliveryPending   = "pending"
	deliverySent      = "sent"
	deliveryCancelled = "cancelled"
)

// PendingDelivery is one item waiting to be sent to one chat.
type PendingDelivery struct {
	Source Source
	Item   FeedItem
	ChatID string
}

// StoreStats is a summary for the /status command.
type StoreStats struct {
	Sources      int
	ActiveSource int
	Destinations int
	Pending      int
	Sent         int
	Errors       []Source
}

// Store persists the Python bot's SQLite schema. Keeping the schema and column names
// compatible allows the Go and Python versions to reuse data/state.db.
type Store struct {
	mu  sync.Mutex
	db  *sql.DB
	now func() time.Time
}

type rowScanner interface {
	Scan(dest ...any) error
}

func OpenStore(path string) (*Store, error) {
	if path == "" {
		path = "./data/state.db"
	}
	if path != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, fmt.Errorf("create database directory: %w", err)
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open SQLite database: %w", err)
	}
	// The bot uses a single serialized store. Keeping one connection also ensures
	// connection-scoped PRAGMAs remain in effect for all database operations.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	store := &Store{db: db, now: time.Now}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("open SQLite database %q: %w", path, err)
	}
	for _, statement := range []string{
		"PRAGMA foreign_keys=ON",
		"PRAGMA busy_timeout=5000",
		"PRAGMA journal_mode=WAL",
	} {
		if _, err := db.Exec(statement); err != nil {
			db.Close()
			return nil, fmt.Errorf("configure SQLite database: %w", err)
		}
	}
	if err := store.initSchema(); err != nil {
		db.Close()
		return nil, err
	}
	return store, nil
}

func (s *Store) initSchema() error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS sources (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			uid TEXT NOT NULL UNIQUE,
			name TEXT NOT NULL,
			feedUrl TEXT NOT NULL DEFAULT '',
			active INTEGER NOT NULL DEFAULT 1,
			initialized INTEGER NOT NULL DEFAULT 0,
			lastChecked TEXT,
			lastError TEXT,
			createdAt TEXT NOT NULL,
			commentsInitialized INTEGER NOT NULL DEFAULT 0,
			lastCommentsChecked TEXT,
			lastCommentError TEXT,
			kind TEXT NOT NULL DEFAULT 'test'
		)`,
		`CREATE TABLE IF NOT EXISTS posts (
			sourceId INTEGER NOT NULL,
			itemKey TEXT NOT NULL,
			payloadJson TEXT NOT NULL,
			status TEXT NOT NULL DEFAULT 'pending',
			attempts INTEGER NOT NULL DEFAULT 0,
			nextAttemptAt TEXT,
			discoveredAt TEXT NOT NULL,
			sentAt TEXT,
			lastError TEXT,
			PRIMARY KEY (sourceId, itemKey),
			FOREIGN KEY (sourceId) REFERENCES sources(id) ON DELETE CASCADE
		)`,
		`CREATE INDEX IF NOT EXISTS idxPostsPending ON posts(status, nextAttemptAt, discoveredAt)`,
		`CREATE TABLE IF NOT EXISTS destinations (
			chatId TEXT PRIMARY KEY,
			name TEXT NOT NULL,
			active INTEGER NOT NULL DEFAULT 1,
			addedAt TEXT NOT NULL,
			kind TEXT NOT NULL DEFAULT 'test'
		)`,
		`CREATE TABLE IF NOT EXISTS deliveries (
			sourceId INTEGER NOT NULL,
			itemKey TEXT NOT NULL,
			chatId TEXT NOT NULL,
			status TEXT NOT NULL DEFAULT 'pending',
			attempts INTEGER NOT NULL DEFAULT 0,
			nextAttemptAt TEXT,
			sentAt TEXT,
			lastError TEXT,
			PRIMARY KEY (sourceId, itemKey, chatId),
			FOREIGN KEY (sourceId, itemKey) REFERENCES posts(sourceId, itemKey) ON DELETE CASCADE
		)`,
		`CREATE INDEX IF NOT EXISTS idxDeliveriesPending ON deliveries(status, nextAttemptAt, sourceId)`,
		`CREATE TABLE IF NOT EXISTS settings (key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
		`INSERT OR IGNORE INTO settings(key, value) VALUES('paused', '0')`,
	}
	for _, statement := range statements {
		if _, err := s.db.Exec(statement); err != nil {
			return fmt.Errorf("initialize SQLite schema: %w", err)
		}
	}
	// Upgrade the earlier Python SQLite schema in place. Existing account and delivery
	// records are retained; newly introduced kinds default to test for safety.
	for _, column := range []struct {
		table string
		name  string
		def   string
	}{
		{"sources", "commentsInitialized", "INTEGER NOT NULL DEFAULT 0"},
		{"sources", "lastCommentsChecked", "TEXT"},
		{"sources", "lastCommentError", "TEXT"},
		{"sources", "kind", "TEXT NOT NULL DEFAULT 'test'"},
		{"destinations", "kind", "TEXT NOT NULL DEFAULT 'test'"},
	} {
		if err := s.ensureColumn(column.table, column.name, column.def); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) ensureColumn(table, column, definition string) error {
	// Table and column names are internal constants above, not user input.
	rows, err := s.db.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		return fmt.Errorf("inspect SQLite schema: %w", err)
	}
	found := false
	for rows.Next() {
		var cid int
		var name, kind string
		var notNull, primaryKey int
		var defaultValue any
		if err := rows.Scan(&cid, &name, &kind, &notNull, &defaultValue, &primaryKey); err != nil {
			rows.Close()
			return fmt.Errorf("read SQLite schema: %w", err)
		}
		if name == column {
			found = true
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	if !found {
		if _, err := s.db.Exec("ALTER TABLE " + table + " ADD COLUMN " + column + " " + definition); err != nil {
			return fmt.Errorf("upgrade SQLite column %s.%s: %w", table, column, err)
		}
	}
	return nil
}

func (s *Store) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db != nil {
		_ = s.db.Close()
		s.db = nil
	}
}

func (s *Store) nowIso() string {
	return s.now().UTC().Format("2006-01-02T15:04:05+00:00")
}

func scanSource(row rowScanner) (Source, error) {
	var source Source
	var active, initialized, commentsInitialized int
	var lastChecked, lastError, lastCommentsChecked, lastCommentError sql.NullString
	err := row.Scan(
		&source.ID, &source.UID, &source.Name, &active, &initialized, &commentsInitialized,
		&lastChecked, &lastError, &lastCommentsChecked, &lastCommentError,
		&source.Kind, &source.CreatedAt,
	)
	if err != nil {
		return Source{}, err
	}
	source.Active = active != 0
	source.Initialized = initialized != 0
	source.CommentsInitialized = commentsInitialized != 0
	source.LastChecked = lastChecked.String
	source.LastError = lastError.String
	source.LastCommentsChecked = lastCommentsChecked.String
	source.LastCommentError = lastCommentError.String
	return source, nil
}

const sourceColumns = `id, uid, name, active, initialized, commentsInitialized,
	lastChecked, lastError, lastCommentsChecked, lastCommentError, kind, createdAt`

const sourceColumnsJoined = `s.id, s.uid, s.name, s.active, s.initialized, s.commentsInitialized,
	s.lastChecked, s.lastError, s.lastCommentsChecked, s.lastCommentError, s.kind, s.createdAt`

func (s *Store) getSourceLocked(key string) (Source, bool) {
	query := `SELECT ` + sourceColumns + ` FROM sources WHERE uid=? OR name=? ORDER BY id LIMIT 1`
	source, err := scanSource(s.db.QueryRow(query, key, key))
	if errors.Is(err, sql.ErrNoRows) {
		return Source{}, false
	}
	if err != nil {
		return Source{}, false
	}
	return source, true
}

func (s *Store) sourceByIDLocked(id int) (Source, bool) {
	source, err := scanSource(s.db.QueryRow(`SELECT `+sourceColumns+` FROM sources WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) || err != nil {
		return Source{}, false
	}
	return source, true
}

func (s *Store) AddSource(uid, name, kind string) (Source, error) {
	if kind == "" {
		kind = kindTest
	}
	if !validKind(kind) {
		return Source{}, errors.New("account kind must be 'public' or 'test'")
	}
	if strings.TrimSpace(name) == "" {
		name = uid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	result, err := s.db.Exec(`INSERT INTO sources(uid, name, feedUrl, createdAt, kind) VALUES(?, ?, '', ?, ?)`, uid, name, s.nowIso(), kind)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") || strings.Contains(strings.ToLower(err.Error()), "constraint") {
			return Source{}, fmt.Errorf("Weibo UID %s already exists", uid)
		}
		return Source{}, err
	}
	id64, err := result.LastInsertId()
	if err != nil {
		return Source{}, err
	}
	returnSource, ok := s.sourceByIDLocked(int(id64))
	if !ok {
		return Source{}, errors.New("new account could not be read back from SQLite")
	}
	return returnSource, nil
}

func (s *Store) GetSource(key string) (Source, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.getSourceLocked(strings.TrimSpace(key))
}

func (s *Store) GetSourceByID(id int) (Source, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sourceByIDLocked(id)
}

func (s *Store) ListSources(activeOnly bool) []Source {
	s.mu.Lock()
	defer s.mu.Unlock()
	query := `SELECT ` + sourceColumns + ` FROM sources`
	if activeOnly {
		query += ` WHERE active=1`
	}
	query += ` ORDER BY id`
	rows, err := s.db.Query(query)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var result []Source
	for rows.Next() {
		source, err := scanSource(rows)
		if err != nil {
			continue
		}
		result = append(result, source)
	}
	return result
}

func (s *Store) RemoveSource(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	result, err := s.db.Exec(`DELETE FROM sources WHERE uid=? OR name=?`, strings.TrimSpace(key), strings.TrimSpace(key))
	if err != nil {
		return false
	}
	count, _ := result.RowsAffected()
	return count > 0
}

func (s *Store) SetSourceActive(key string, active bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	value := 0
	if active {
		value = 1
	}
	result, err := s.db.Exec(`UPDATE sources SET active=? WHERE uid=? OR name=?`, value, strings.TrimSpace(key), strings.TrimSpace(key))
	if err != nil {
		return false
	}
	count, _ := result.RowsAffected()
	return count > 0
}

func (s *Store) RenameSource(id int, name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, _ = s.db.Exec(`UPDATE sources SET name=? WHERE id=?`, name, id)
}

func (s *Store) SetSourceKind(key, kind string) (bool, error) {
	if !validKind(kind) {
		return false, errors.New("account kind must be 'public' or 'test'")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	result, err := tx.Exec(`UPDATE sources SET kind=? WHERE id=(SELECT id FROM sources WHERE uid=? OR name=? ORDER BY id LIMIT 1)`, kind, strings.TrimSpace(key), strings.TrimSpace(key))
	if err != nil {
		return false, err
	}
	count, _ := result.RowsAffected()
	if count == 0 {
		return false, nil
	}
	if kind == kindTest {
		_, err = tx.Exec(`UPDATE deliveries SET status='cancelled' WHERE status='pending' AND sourceId=(SELECT id FROM sources WHERE uid=? OR name=? ORDER BY id LIMIT 1) AND chatId IN (SELECT chatId FROM destinations WHERE kind='public')`, strings.TrimSpace(key), strings.TrimSpace(key))
		if err != nil {
			return false, err
		}
		_, err = tx.Exec(`UPDATE posts SET status='sent', sentAt=? WHERE sourceId=(SELECT id FROM sources WHERE uid=? OR name=? ORDER BY id LIMIT 1) AND status='pending' AND NOT EXISTS (SELECT 1 FROM deliveries d WHERE d.sourceId=posts.sourceId AND d.itemKey=posts.itemKey AND d.status='pending')`, s.nowIso(), strings.TrimSpace(key), strings.TrimSpace(key))
		if err != nil {
			return false, err
		}
	}
	return true, tx.Commit()
}

func (s *Store) SetInitialized(id int, value bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	flag := 0
	if value {
		flag = 1
	}
	_, _ = s.db.Exec(`UPDATE sources SET initialized=? WHERE id=?`, flag, id)
}

func (s *Store) SetCommentsInitialized(id int, value bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	flag := 0
	if value {
		flag = 1
	}
	_, _ = s.db.Exec(`UPDATE sources SET commentsInitialized=? WHERE id=?`, flag, id)
}

func (s *Store) MarkChecked(id int, errText string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, _ = s.db.Exec(`UPDATE sources SET lastChecked=?, lastError=? WHERE id=?`, s.nowIso(), nullableError(errText), id)
}

func (s *Store) MarkCommentsChecked(id int, errText string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, _ = s.db.Exec(`UPDATE sources SET lastCommentsChecked=?, lastCommentError=? WHERE id=?`, s.nowIso(), nullableError(errText), id)
}

func nullableError(value string) any {
	if value == "" {
		return nil
	}
	return truncateRunes(value, 1000)
}

func (s *Store) CommentsDue(source Source, interval time.Duration) bool {
	if source.LastCommentsChecked == "" {
		return true
	}
	last, err := time.Parse(time.RFC3339Nano, source.LastCommentsChecked)
	if err != nil {
		return true
	}
	return s.now().Sub(last) >= interval
}

func (s *Store) AddDestination(chatID, name, kind string) error {
	chatID = strings.TrimSpace(chatID)
	name = strings.TrimSpace(name)
	if chatID == "" {
		return errors.New("chat id must not be empty")
	}
	if kind != "" && !validKind(kind) {
		return errors.New("destination kind must be 'public' or 'test'")
	}
	if name == "" {
		name = chatID
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var existingKind string
	err = tx.QueryRow(`SELECT kind FROM destinations WHERE chatId=?`, chatID).Scan(&existingKind)
	if errors.Is(err, sql.ErrNoRows) {
		storedKind := kind
		if storedKind == "" {
			storedKind = kindTest
		}
		_, err = tx.Exec(`INSERT INTO destinations(chatId,name,active,addedAt,kind) VALUES(?,?,1,?,?)`, chatID, name, s.nowIso(), storedKind)
	} else if err == nil {
		if kind == "" {
			kind = existingKind
		}
		_, err = tx.Exec(`UPDATE destinations SET name=?,active=1,kind=? WHERE chatId=?`, name, kind, chatID)
	}
	if err != nil {
		return err
	}
	var destinationKind string
	if err = tx.QueryRow(`SELECT kind FROM destinations WHERE chatId=?`, chatID).Scan(&destinationKind); err != nil {
		return err
	}
	rows, err := tx.Query(`SELECT p.sourceId,p.itemKey,s.kind FROM posts p JOIN sources s ON s.id=p.sourceId WHERE p.status='pending' AND (?='test' OR s.kind='public')`, destinationKind)
	if err != nil {
		return err
	}
	type queuedItem struct {
		sourceID   int
		itemKey    string
		sourceKind string
	}
	var pending []queuedItem
	for rows.Next() {
		var item queuedItem
		if err := rows.Scan(&item.sourceID, &item.itemKey, &item.sourceKind); err != nil {
			rows.Close()
			return err
		}
		pending = append(pending, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, item := range pending {
		_, err = tx.Exec(`INSERT OR IGNORE INTO deliveries(sourceId,itemKey,chatId,status) VALUES(?,?,?,'pending')`, item.sourceID, item.itemKey, chatID)
		if err != nil {
			return err
		}
		_, err = tx.Exec(`UPDATE deliveries SET status='pending',attempts=0,nextAttemptAt=NULL,lastError=NULL WHERE sourceId=? AND itemKey=? AND chatId=? AND status='cancelled'`, item.sourceID, item.itemKey, chatID)
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) RemoveDestination(chatID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return false
	}
	defer tx.Rollback()
	result, err := tx.Exec(`DELETE FROM destinations WHERE chatId=?`, strings.TrimSpace(chatID))
	if err != nil {
		return false
	}
	count, _ := result.RowsAffected()
	if count == 0 {
		return false
	}
	if _, err := tx.Exec(`UPDATE deliveries SET status='cancelled' WHERE chatId=? AND status='pending'`, strings.TrimSpace(chatID)); err != nil {
		return false
	}
	return tx.Commit() == nil
}

func (s *Store) SetDestinationKind(chatID, kind string) (bool, error) {
	if !validKind(kind) {
		return false, errors.New("destination kind must be 'public' or 'test'")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	result, err := s.db.Exec(`UPDATE destinations SET kind=? WHERE chatId=?`, kind, strings.TrimSpace(chatID))
	if err != nil {
		return false, err
	}
	count, _ := result.RowsAffected()
	return count > 0, nil
}

func (s *Store) Destinations(kind string) []Destination {
	s.mu.Lock()
	defer s.mu.Unlock()
	query := `SELECT chatId,name,addedAt,kind FROM destinations WHERE active=1`
	args := []any{}
	if kind != "" {
		query += ` AND kind=?`
		args = append(args, kind)
	}
	query += ` ORDER BY addedAt`
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var result []Destination
	for rows.Next() {
		var destination Destination
		if err := rows.Scan(&destination.ChatID, &destination.Name, &destination.AddedAt, &destination.Kind); err == nil {
			result = append(result, destination)
		}
	}
	return result
}

func (s *Store) DestinationKinds() map[string]string {
	result := map[string]string{}
	for _, destination := range s.Destinations("") {
		result[destination.ChatID] = destination.Kind
	}
	return result
}

func (s *Store) InsertItems(sourceID int, items []FeedItem, skip bool) int {
	if len(items) == 0 {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return 0
	}
	defer tx.Rollback()
	var sourceKind string
	if err := tx.QueryRow(`SELECT kind FROM sources WHERE id=?`, sourceID).Scan(&sourceKind); err != nil {
		return 0
	}
	type target struct {
		chatID string
		kind   string
	}
	rows, err := tx.Query(`SELECT chatId,kind FROM destinations WHERE active=1 ORDER BY addedAt`)
	if err != nil {
		return 0
	}
	var targets []target
	for rows.Next() {
		var target target
		if err := rows.Scan(&target.chatID, &target.kind); err != nil {
			rows.Close()
			return 0
		}
		targets = append(targets, target)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0
	}
	rows.Close()
	status := postPending
	if skip {
		status = postSkipped
	}
	inserted := 0
	for _, item := range items {
		payload, err := json.Marshal(item)
		if err != nil {
			continue
		}
		result, err := tx.Exec(`INSERT OR IGNORE INTO posts(sourceId,itemKey,payloadJson,status,discoveredAt) VALUES(?,?,?,?,?)`, sourceID, item.ItemKey, string(payload), status, s.nowIso())
		if err != nil {
			return 0
		}
		count, _ := result.RowsAffected()
		if count == 0 {
			continue
		}
		inserted++
		if skip {
			continue
		}
		for _, destination := range targets {
			if sourceKind != kindPublic && destination.kind != kindTest {
				continue
			}
			if _, err := tx.Exec(`INSERT OR IGNORE INTO deliveries(sourceId,itemKey,chatId,status) VALUES(?,?,?,'pending')`, sourceID, item.ItemKey, destination.chatID); err != nil {
				return 0
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return 0
	}
	return inserted
}

func (s *Store) BackfillSkipped(sourceID, count int) int {
	if count <= 0 {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return 0
	}
	defer tx.Rollback()
	var sourceKind string
	if err := tx.QueryRow(`SELECT kind FROM sources WHERE id=?`, sourceID).Scan(&sourceKind); err != nil {
		return 0
	}
	rows, err := tx.Query(`SELECT itemKey FROM posts WHERE sourceId=? AND status='skipped' ORDER BY discoveredAt DESC LIMIT ?`, sourceID, count)
	if err != nil {
		return 0
	}
	var keys []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err == nil {
			keys = append(keys, key)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0
	}
	rows.Close()
	if len(keys) == 0 {
		return 0
	}
	type target struct{ chatID, kind string }
	destRows, err := tx.Query(`SELECT chatId,kind FROM destinations WHERE active=1 ORDER BY addedAt`)
	if err != nil {
		return 0
	}
	var targets []target
	for destRows.Next() {
		var item target
		if err := destRows.Scan(&item.chatID, &item.kind); err != nil {
			destRows.Close()
			return 0
		}
		targets = append(targets, item)
	}
	destRows.Close()
	updated := 0
	for _, key := range keys {
		result, err := tx.Exec(`UPDATE posts SET status='pending', attempts=0, nextAttemptAt=NULL, lastError=NULL WHERE sourceId=? AND itemKey=? AND status='skipped'`, sourceID, key)
		if err != nil {
			return 0
		}
		count, _ := result.RowsAffected()
		if count == 0 {
			continue
		}
		updated++
		if _, err := tx.Exec(`DELETE FROM deliveries WHERE sourceId=? AND itemKey=?`, sourceID, key); err != nil {
			return 0
		}
		for _, dest := range targets {
			if sourceKind != kindPublic && dest.kind != kindTest {
				continue
			}
			if _, err := tx.Exec(`INSERT INTO deliveries(sourceId,itemKey,chatId,status) VALUES(?,?,?,'pending')`, sourceID, key, dest.chatID); err != nil {
				return 0
			}
		}
	}
	if tx.Commit() != nil {
		return 0
	}
	return updated
}

func (s *Store) PendingDeliveries(sourceID, limit int) []PendingDelivery {
	if limit <= 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	query := `SELECT ` + sourceColumnsJoined + `,p.payloadJson,d.chatId
		FROM deliveries d JOIN posts p ON p.sourceId=d.sourceId AND p.itemKey=d.itemKey
		JOIN destinations t ON t.chatId=d.chatId AND t.active=1
		JOIN sources s ON s.id=d.sourceId
		WHERE (s.kind='public' OR t.kind='test') AND d.status='pending'
		AND (d.nextAttemptAt IS NULL OR d.nextAttemptAt<=?)`
	args := []any{s.nowIso()}
	if sourceID != 0 {
		query += ` AND d.sourceId=?`
		args = append(args, sourceID)
	}
	query += ` ORDER BY p.discoveredAt ASC, p.sourceId ASC, p.itemKey ASC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var result []PendingDelivery
	for rows.Next() {
		var source Source
		var active, initialized, commentsInitialized int
		var lastChecked, lastError, lastCommentsChecked, lastCommentError sql.NullString
		var payload, chatID string
		err := rows.Scan(
			&source.ID, &source.UID, &source.Name, &active, &initialized, &commentsInitialized,
			&lastChecked, &lastError, &lastCommentsChecked, &lastCommentError, &source.Kind, &source.CreatedAt,
			&payload, &chatID,
		)
		if err != nil {
			continue
		}
		source.Active = active != 0
		source.Initialized = initialized != 0
		source.CommentsInitialized = commentsInitialized != 0
		source.LastChecked = lastChecked.String
		source.LastError = lastError.String
		source.LastCommentsChecked = lastCommentsChecked.String
		source.LastCommentError = lastCommentError.String
		var item FeedItem
		if err := json.Unmarshal([]byte(payload), &item); err != nil {
			continue
		}
		result = append(result, PendingDelivery{Source: source, Item: item, ChatID: chatID})
	}
	return result
}

func (s *Store) MarkDeliverySent(sourceID int, itemKey, chatID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return
	}
	defer tx.Rollback()
	_, err = tx.Exec(`UPDATE deliveries SET status='sent',sentAt=?,lastError=NULL,nextAttemptAt=NULL WHERE sourceId=? AND itemKey=? AND chatId=?`, s.nowIso(), sourceID, itemKey, chatID)
	if err != nil {
		return
	}
	var remaining int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM deliveries WHERE sourceId=? AND itemKey=? AND status='pending'`, sourceID, itemKey).Scan(&remaining); err != nil {
		return
	}
	if remaining == 0 {
		if _, err := tx.Exec(`UPDATE posts SET status='sent',sentAt=?,lastError=NULL,nextAttemptAt=NULL WHERE sourceId=? AND itemKey=?`, s.nowIso(), sourceID, itemKey); err != nil {
			return
		}
	}
	_ = tx.Commit()
}

func (s *Store) MarkDeliveryFailed(sourceID int, itemKey, chatID, errText string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return
	}
	defer tx.Rollback()
	var attempts int
	if err := tx.QueryRow(`SELECT attempts FROM deliveries WHERE sourceId=? AND itemKey=? AND chatId=?`, sourceID, itemKey, chatID).Scan(&attempts); err != nil {
		return
	}
	attempts++
	exponent := attempts - 1
	if exponent > 7 {
		exponent = 7
	}
	delay := 30 * time.Second * time.Duration(1<<exponent)
	if delay > time.Hour {
		delay = time.Hour
	}
	retryAt := s.now().Add(delay).UTC().Format("2006-01-02T15:04:05+00:00")
	errText = truncateRunes(errText, 1000)
	if _, err := tx.Exec(`UPDATE deliveries SET attempts=?,nextAttemptAt=?,lastError=? WHERE sourceId=? AND itemKey=? AND chatId=?`, attempts, retryAt, errText, sourceID, itemKey, chatID); err != nil {
		return
	}
	if _, err := tx.Exec(`UPDATE posts SET lastError=? WHERE sourceId=? AND itemKey=?`, errText, sourceID, itemKey); err != nil {
		return
	}
	_ = tx.Commit()
}

func (s *Store) SetPaused(paused bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	value := "0"
	if paused {
		value = "1"
	}
	_, _ = s.db.Exec(`INSERT INTO settings(key,value) VALUES('paused',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, value)
}

func (s *Store) IsPaused() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	var value string
	if err := s.db.QueryRow(`SELECT value FROM settings WHERE key='paused'`).Scan(&value); err != nil {
		return false
	}
	return value == "1"
}

func (s *Store) Stats() StoreStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	var stats StoreStats
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM sources`).Scan(&stats.Sources)
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM sources WHERE active=1`).Scan(&stats.ActiveSource)
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM destinations WHERE active=1`).Scan(&stats.Destinations)
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM deliveries WHERE status='pending'`).Scan(&stats.Pending)
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM deliveries WHERE status='sent'`).Scan(&stats.Sent)
	rows, err := s.db.Query(`SELECT ` + sourceColumns + ` FROM sources WHERE lastError IS NOT NULL OR lastCommentError IS NOT NULL ORDER BY lastChecked DESC LIMIT 5`)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			if source, err := scanSource(rows); err == nil {
				stats.Errors = append(stats.Errors, source)
			}
		}
	}
	return stats
}

func truncateRunes(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit])
}
