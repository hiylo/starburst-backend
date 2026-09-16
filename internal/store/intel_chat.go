package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// IntelChat is one project-level conversation thread for the knowledge base
// Q&A. It groups the user/assistant turns of a single dialog so multi-turn
// context can be reconstructed and reviewed later.
type IntelChat struct {
	ID        int64     `json:"id"`
	ProjectID int64     `json:"projectId"`
	Title     string    `json:"title"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// IntelChatMessage is a single turn in a conversation. Assistant messages may
// carry the retrieval sources (provenance) as a JSON array in SourcesJSON.
type IntelChatMessage struct {
	ID          int64     `json:"id"`
	ChatID      int64     `json:"chatId"`
	Role        string    `json:"role"`
	Content     string    `json:"content"`
	SourcesJSON string    `json:"sourcesJson,omitempty"`
	CreatedAt   time.Time `json:"createdAt"`
}

// CreateIntelChat persists a new conversation and populates its id.
func (s *sqlStore) CreateIntelChat(ctx context.Context, c *IntelChat) error {
	if isPostgres(s.driver) {
		return s.db.QueryRowContext(ctx, s.q(`
			INSERT INTO intel_chats (project_id, title, created_at, updated_at)
			VALUES (?, ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)
			RETURNING id`),
			c.ProjectID, c.Title,
		).Scan(&c.ID)
	}
	res, err := s.db.ExecContext(ctx, s.q(`
		INSERT INTO intel_chats (project_id, title, created_at, updated_at)
		VALUES (?, ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`),
		c.ProjectID, c.Title)
	if err != nil {
		return err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return err
	}
	c.ID = id
	return nil
}

// ListIntelChats returns a project's conversations, newest first.
func (s *sqlStore) ListIntelChats(ctx context.Context, projectID int64) ([]*IntelChat, error) {
	rows, err := s.db.QueryContext(ctx, s.q(`
		SELECT id, project_id, title, created_at, updated_at
		FROM intel_chats WHERE project_id = ? ORDER BY updated_at DESC`), projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*IntelChat, 0)
	for rows.Next() {
		c := &IntelChat{}
		if err := rows.Scan(&c.ID, &c.ProjectID, &c.Title, &c.CreatedAt, &c.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// GetIntelChat loads a single conversation.
func (s *sqlStore) GetIntelChat(ctx context.Context, id int64) (*IntelChat, error) {
	row := s.db.QueryRowContext(ctx, s.q(`
		SELECT id, project_id, title, created_at, updated_at
		FROM intel_chats WHERE id = ?`), id)
	c := &IntelChat{}
	if err := row.Scan(&c.ID, &c.ProjectID, &c.Title, &c.CreatedAt, &c.UpdatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return c, nil
}

// DeleteIntelChat removes a conversation and its messages.
func (s *sqlStore) DeleteIntelChat(ctx context.Context, id int64) error {
	if _, err := s.db.ExecContext(ctx, s.q(`DELETE FROM intel_chat_messages WHERE chat_id = ?`), id); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, s.q(`DELETE FROM intel_chats WHERE id = ?`), id); err != nil {
		return err
	}
	return nil
}

// AddIntelChatMessage appends a message and touches the conversation's
// updated_at so the list reorders.
func (s *sqlStore) AddIntelChatMessage(ctx context.Context, m *IntelChatMessage) error {
	if isPostgres(s.driver) {
		err := s.db.QueryRowContext(ctx, s.q(`
			INSERT INTO intel_chat_messages (chat_id, role, content, sources_json, created_at)
			VALUES (?, ?, ?, ?, CURRENT_TIMESTAMP)
			RETURNING id`),
			m.ChatID, m.Role, m.Content, m.SourcesJSON,
		).Scan(&m.ID)
		if err != nil {
			return err
		}
	} else {
		res, err := s.db.ExecContext(ctx, s.q(`
			INSERT INTO intel_chat_messages (chat_id, role, content, sources_json, created_at)
			VALUES (?, ?, ?, ?, CURRENT_TIMESTAMP)`),
			m.ChatID, m.Role, m.Content, m.SourcesJSON)
		if err != nil {
			return err
		}
		id, err := res.LastInsertId()
		if err != nil {
			return err
		}
		m.ID = id
	}
	_, err := s.db.ExecContext(ctx, s.q(
		`UPDATE intel_chats SET updated_at = CURRENT_TIMESTAMP WHERE id = ?`), m.ChatID)
	return err
}

// ListIntelChatMessages returns a conversation's messages in chronological
// order. When limit > 0 only the most recent `limit` messages are returned
// (still in chronological order); limit 0 returns all.
func (s *sqlStore) ListIntelChatMessages(ctx context.Context, chatID int64, limit int) ([]*IntelChatMessage, error) {
	order := "ASC"
	if limit > 0 {
		order = "DESC"
	}
	query := `SELECT id, chat_id, role, content, sources_json, created_at
		FROM intel_chat_messages WHERE chat_id = ? ORDER BY id ` + order
	args := []any{chatID}
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := s.db.QueryContext(ctx, s.q(query), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*IntelChatMessage, 0)
	for rows.Next() {
		m := &IntelChatMessage{}
		if err := rows.Scan(&m.ID, &m.ChatID, &m.Role, &m.Content, &m.SourcesJSON, &m.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// When querying DESC (most-recent-first), reverse back to chronological.
	if order == "DESC" {
		for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
			out[i], out[j] = out[j], out[i]
		}
	}
	return out, nil
}
