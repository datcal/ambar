// Package audit appends to the audit_log table (§11).
//
// M0 records login attempts. Later milestones add token creation, metadata
// edits and — most importantly — removals (§9.1, "every removal goes in the
// audit log with the reason and the finding that motivated it").
//
// Nothing reads this table yet. That is deliberate: the record has to exist
// from the first login onward, or the history has a hole in it that no later
// milestone can fill.
package audit

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/datcal/ambar/internal/db"
)

// Actions recorded in M0.
const (
	ActionLoginSucceeded = "login.succeeded"
	ActionLoginFailed    = "login.failed"
	ActionLogout         = "logout"
	ActionUserCreated    = "user.created"
)

// Entry is one audit record. UserID is nil when the actor is unknown, which is
// the normal case for a failed login against a username that does not exist.
type Entry struct {
	UserID   *int64
	Action   string
	Entity   string
	EntityID string
	Detail   any
	IP       string
}

// Logger writes audit entries.
type Logger struct {
	db  *db.DB
	log *slog.Logger
}

func New(d *db.DB, log *slog.Logger) *Logger {
	return &Logger{db: d, log: log}
}

// Record appends an entry.
//
// Audit failures must not break the operation being audited — a login that
// succeeded has succeeded whether or not the row was written. So the error is
// logged and swallowed rather than propagated, which is the one place in this
// codebase where that is the right call.
func (l *Logger) Record(ctx context.Context, e Entry) {
	if err := l.record(ctx, e); err != nil {
		l.log.ErrorContext(ctx, "could not write audit entry",
			"action", e.Action, "error", err)
	}
}

func (l *Logger) record(ctx context.Context, e Entry) error {
	if e.Action == "" {
		return fmt.Errorf("audit entry has no action")
	}

	detail := "{}"
	if e.Detail != nil {
		b, err := json.Marshal(e.Detail)
		if err != nil {
			return fmt.Errorf("marshal detail: %w", err)
		}
		detail = string(b)
	}

	var userID any
	if e.UserID != nil {
		userID = *e.UserID
	}

	if _, err := l.db.Writer.ExecContext(ctx, `
		INSERT INTO audit_log (user_id, action, entity, entity_id, detail_json, ip, at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		userID, e.Action, e.Entity, e.EntityID, detail, e.IP, time.Now().Unix(),
	); err != nil {
		return fmt.Errorf("insert audit_log: %w", err)
	}
	return nil
}
