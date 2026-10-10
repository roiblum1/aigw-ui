package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// keepAudit is how many audit entries are kept. Older ones are removed.
const keepAudit = 20000

// AuditEntry is one request that changed something, or tried to.
type AuditEntry struct {
	ID int64     `json:"id"`
	At time.Time `json:"at"`
	// Actor is how the caller was authenticated.
	Actor string `json:"actor"`
	// OnBehalfOf is the name the caller gave. It is not verified.
	OnBehalfOf string `json:"on_behalf_of"`
	Method     string `json:"method"`
	Path       string `json:"path"`
	Status     int    `json:"status"`
	// Action and Summary say what the request did, in the words of the task
	// log. Both are empty for a request that was refused before it did anything.
	Action  string `json:"action"`
	Summary string `json:"summary"`
	// RemoteAddr is the peer of the connection, which is the router when the
	// server runs behind one. ForwardedFor is the X-Forwarded-For header.
	RemoteAddr   string `json:"remote_addr"`
	ForwardedFor string `json:"forwarded_for"`
	UserAgent    string `json:"user_agent"`
	DurationMS   int    `json:"duration_ms"`
}

func (s *Store) AddAudit(ctx context.Context, e AuditEntry) error {
	return pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO audit_log (actor, on_behalf_of, method, path, status, action, summary, remote_addr, forwarded_for, user_agent, duration_ms)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
			e.Actor, e.OnBehalfOf, e.Method, e.Path, e.Status, e.Action, e.Summary, e.RemoteAddr, e.ForwardedFor, e.UserAgent, e.DurationMS)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `DELETE FROM audit_log WHERE id <= (SELECT max(id) FROM audit_log) - $1`, keepAudit)
		return err
	})
}

// ListAudit returns the newest entries first. before, when not 0, limits it
// to entries older than that ID, for paging.
func (s *Store) ListAudit(ctx context.Context, limit int, before int64) ([]AuditEntry, error) {
	return list(ctx, s, func(r scanner) (AuditEntry, error) {
		var e AuditEntry
		err := r.Scan(&e.ID, &e.At, &e.Actor, &e.OnBehalfOf, &e.Method, &e.Path, &e.Status, &e.Action, &e.Summary,
			&e.RemoteAddr, &e.ForwardedFor, &e.UserAgent, &e.DurationMS)
		return e, err
	},
		`SELECT id, at, actor, on_behalf_of, method, path, status, action, summary, remote_addr, forwarded_for, user_agent, duration_ms
		 FROM audit_log WHERE $2 = 0 OR id < $2 ORDER BY id DESC LIMIT $1`, limit, before)
}
