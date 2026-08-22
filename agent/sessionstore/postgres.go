package sessionstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	_ "github.com/lib/pq"
)

const createSessionsTable = `
CREATE TABLE IF NOT EXISTS sessions (
    id VARCHAR(64) PRIMARY KEY,
    model VARCHAR(128) NOT NULL,
    history JSONB NOT NULL DEFAULT '[]',
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL
);

CREATE TABLE IF NOT EXISTS session_locks (
    session_id VARCHAR(64) PRIMARY KEY REFERENCES sessions(id) ON DELETE CASCADE,
    device_id VARCHAR(128) NOT NULL,
    locked_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_session_locks_expires ON session_locks(expires_at);
`

type Postgres struct {
	db       *sql.DB
	deviceID string
}

func NewPostgres(dsn, deviceID string) (*Postgres, error) {
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, fmt.Errorf("连接数据库失败: %w", err)
	}
	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("数据库连接测试失败: %w", err)
	}
	if _, err := db.Exec(createSessionsTable); err != nil {
		return nil, fmt.Errorf("创建表失败: %w", err)
	}
	return &Postgres{db: db, deviceID: deviceID}, nil
}

func (s *Postgres) Save(ctx context.Context, sess *Session) error {
	history, err := json.Marshal(sess.History)
	if err != nil {
		return fmt.Errorf("序列化历史失败: %w", err)
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO sessions (id, model, history, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (id) DO UPDATE SET
			model = EXCLUDED.model,
			history = EXCLUDED.history,
			updated_at = EXCLUDED.updated_at
	`, sess.ID, sess.Model, history, sess.CreatedAt, sess.UpdatedAt)
	return err
}

func (s *Postgres) Load(ctx context.Context, id string) (*Session, error) {
	var sess Session
	var history []byte
	err := s.db.QueryRowContext(ctx, `
		SELECT id, model, history, created_at, updated_at
		FROM sessions WHERE id = $1
	`, id).Scan(&sess.ID, &sess.Model, &history, &sess.CreatedAt, &sess.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(history, &sess.History); err != nil {
		return nil, fmt.Errorf("反序列化历史失败: %w", err)
	}
	return &sess, nil
}

func (s *Postgres) Delete(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE id = $1`, id)
	return err
}

func (s *Postgres) List(ctx context.Context) ([]*Session, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, model, history, created_at, updated_at
		FROM sessions ORDER BY updated_at ASC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var sessions []*Session
	for rows.Next() {
		var sess Session
		var history []byte
		if err := rows.Scan(&sess.ID, &sess.Model, &history, &sess.CreatedAt, &sess.UpdatedAt); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(history, &sess.History); err != nil {
			return nil, fmt.Errorf("反序列化历史失败: %w", err)
		}
		sessions = append(sessions, &sess)
	}
	return sessions, rows.Err()
}

func (s *Postgres) Lock(ctx context.Context, sessionID string, ttl time.Duration) (bool, error) {
	now := time.Now()
	expiresAt := now.Add(ttl)
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO session_locks (session_id, device_id, locked_at, expires_at)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (session_id) DO UPDATE SET
			device_id = CASE
				WHEN session_locks.expires_at < $3 THEN EXCLUDED.device_id
				ELSE session_locks.device_id
			END,
			locked_at = CASE
				WHEN session_locks.expires_at < $3 THEN EXCLUDED.locked_at
				ELSE session_locks.locked_at
			END,
			expires_at = CASE
				WHEN session_locks.expires_at < $3 THEN EXCLUDED.expires_at
				ELSE session_locks.expires_at
			END
	`, sessionID, s.deviceID, now, expiresAt)
	if err != nil {
		return false, err
	}
	var lockedDevice string
	err = s.db.QueryRowContext(ctx, `
		SELECT device_id FROM session_locks WHERE session_id = $1
	`, sessionID).Scan(&lockedDevice)
	if err != nil {
		return false, err
	}
	return lockedDevice == s.deviceID, nil
}

func (s *Postgres) Unlock(ctx context.Context, sessionID string) error {
	_, err := s.db.ExecContext(ctx, `
		DELETE FROM session_locks
		WHERE session_id = $1 AND device_id = $2
	`, sessionID, s.deviceID)
	return err
}

func (s *Postgres) Renew(ctx context.Context, sessionID string, ttl time.Duration) (bool, error) {
	expiresAt := time.Now().Add(ttl)
	result, err := s.db.ExecContext(ctx, `
		UPDATE session_locks
		SET expires_at = $1
		WHERE session_id = $2 AND device_id = $3 AND expires_at > NOW()
	`, expiresAt, sessionID, s.deviceID)
	if err != nil {
		return false, err
	}
	n, _ := result.RowsAffected()
	return n > 0, nil
}

func (s *Postgres) IsLocked(ctx context.Context, sessionID string) (bool, string, error) {
	var deviceID string
	var expiresAt time.Time
	err := s.db.QueryRowContext(ctx, `
		SELECT device_id, expires_at FROM session_locks WHERE session_id = $1
	`, sessionID).Scan(&deviceID, &expiresAt)
	if err == sql.ErrNoRows {
		return false, "", nil
	}
	if err != nil {
		return false, "", err
	}
	if time.Now().After(expiresAt) {
		return false, "", nil
	}
	return true, deviceID, nil
}

func (s *Postgres) CleanExpired(ctx context.Context) (int64, error) {
	result, err := s.db.ExecContext(ctx, `
		DELETE FROM session_locks WHERE expires_at < NOW()
	`)
	if err != nil {
		return 0, err
	}
	n, _ := result.RowsAffected()
	return n, nil
}

func (s *Postgres) Close() error {
	return s.db.Close()
}

var _ Store = (*Postgres)(nil)

type LockManager struct {
	store     *Postgres
	sessionID string
	ttl       time.Duration
	interval  time.Duration
	mu        sync.Mutex
	cancel    context.CancelFunc
	locked    bool
}

func NewLockManager(store *Postgres, sessionID string, ttl, interval time.Duration) *LockManager {
	return &LockManager{
		store:     store,
		sessionID: sessionID,
		ttl:       ttl,
		interval:  interval,
	}
}

func (m *LockManager) Acquire(ctx context.Context) (bool, error) {
	acquired, err := m.store.Lock(ctx, m.sessionID, m.ttl)
	if err != nil {
		return false, err
	}
	if !acquired {
		return false, nil
	}
	m.locked = true
	go m.renewLoop()
	return true, nil
}

func (m *LockManager) renewLoop() {
	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			m.mu.Lock()
			if !m.locked {
				m.mu.Unlock()
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			renewed, err := m.store.Renew(ctx, m.sessionID, m.ttl)
			cancel()
			if err != nil || !renewed {
				m.locked = false
				m.mu.Unlock()
				return
			}
			m.mu.Unlock()
		}
	}
}

func (m *LockManager) Release(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.locked {
		return nil
	}
	err := m.store.Unlock(ctx, m.sessionID)
	m.locked = false
	return err
}

func (m *LockManager) IsHeld() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.locked
}
