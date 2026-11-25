package db

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// The dashboard assistant (25 Nov 2026, P1: read-only, super admins, dev): every message
// is kept here, per user and conversation, so the server holds the history the model sees
// (never the browser), the rate limits count across both server copies, and a super admin
// can read back what was asked and answered. Tool rows record which lookup ran and how big
// its answer was; their content is not kept. Pruned after 90 days (PruneAssistant).
const assistantSchema = `
CREATE TABLE IF NOT EXISTS assistant_messages (
	id          BIGSERIAL PRIMARY KEY,
	conv        UUID NOT NULL,
	username    TEXT NOT NULL,
	role        TEXT NOT NULL CHECK (role IN ('user', 'assistant', 'tool', 'error')),
	content     TEXT NOT NULL DEFAULT '',
	tool        TEXT NOT NULL DEFAULT '',
	page        TEXT NOT NULL DEFAULT '',
	tokens_in   INT NOT NULL DEFAULT 0,
	tokens_out  INT NOT NULL DEFAULT 0,
	created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_assistant_messages_conv ON assistant_messages (conv, id);
CREATE INDEX IF NOT EXISTS idx_assistant_messages_user ON assistant_messages (username, created_at DESC);
`

// AssistantMessage is one stored line of a conversation.
type AssistantMessage struct {
	Role    string
	Content string
}

// AddAssistantMessage stores one line.
func (d *DB) AddAssistantMessage(ctx context.Context, conv uuid.UUID, username, role, content, tool, page string, tokensIn, tokensOut int) error {
	_, err := d.pool.Exec(ctx, `INSERT INTO assistant_messages (conv, username, role, content, tool, page, tokens_in, tokens_out)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`, conv, username, role, content, tool, page, tokensIn, tokensOut)
	return err
}

// AssistantHistory is the last `turns` user/assistant lines of one of this user's
// conversations, oldest first: what the model is given as the conversation so far.
func (d *DB) AssistantHistory(ctx context.Context, conv uuid.UUID, username string, turns int) ([]AssistantMessage, error) {
	rows, err := d.pool.Query(ctx, `SELECT role, content FROM (
		SELECT id, role, content FROM assistant_messages
		 WHERE conv = $1 AND username = $2 AND role IN ('user', 'assistant')
		 ORDER BY id DESC LIMIT $3) t ORDER BY id`, conv, username, turns)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AssistantMessage
	for rows.Next() {
		var m AssistantMessage
		if err := rows.Scan(&m.Role, &m.Content); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// AssistantAskedSince counts the questions a user asked since each time: the rate limits.
func (d *DB) AssistantAskedSince(ctx context.Context, username string, short, long time.Time) (nShort, nLong int, err error) {
	err = d.pool.QueryRow(ctx, `SELECT
		COUNT(*) FILTER (WHERE created_at >= $2), COUNT(*) FILTER (WHERE created_at >= $3)
		FROM assistant_messages WHERE username = $1 AND role = 'user' AND created_at >= $3`,
		username, short, long).Scan(&nShort, &nLong)
	return nShort, nLong, err
}

// PruneAssistant deletes lines older than the cutoff.
func (d *DB) PruneAssistant(ctx context.Context, before time.Time) (int64, error) {
	tag, err := d.pool.Exec(ctx, `DELETE FROM assistant_messages WHERE created_at < $1`, before)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}
