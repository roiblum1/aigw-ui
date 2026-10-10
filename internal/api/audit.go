package api

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"aigw-ui/internal/store"
)

// adminActor names the only way to authenticate today.
const adminActor = "admin token"

// OnBehalfOfHeader lets a caller that shares the admin token, such as the
// self-service portal or a person in the UI, say who is acting. The value is
// recorded as given: nothing verifies it. It may be percent-encoded, which is
// how a name outside ASCII fits in a header.
const OnBehalfOfHeader = "X-On-Behalf-Of"

type auditKey struct{}

// auditNote is filled in by a handler to say what the request did.
type auditNote struct{ action, summary string }

// note records what the request did, for the audit log.
func note(r *http.Request, action, summary string) {
	if n, ok := r.Context().Value(auditKey{}).(*auditNote); ok {
		n.action, n.summary = action, summary
	}
}

// statusWriter remembers the status code a handler answered with.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

// audit records every authenticated request that is not a plain read: who
// sent it, what it did and how it ended. Reads are not recorded; no response
// to a read contains a secret.
func (s *Server) audit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet || r.Method == http.MethodHead {
			next.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		n := &auditNote{}
		sw := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(sw, r.WithContext(context.WithValue(r.Context(), auditKey{}, n)))

		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			host = r.RemoteAddr
		}
		e := store.AuditEntry{
			Actor:        adminActor,
			OnBehalfOf:   clip(onBehalfOf(r), 100),
			Method:       r.Method,
			Path:         clip(r.URL.Path, 300),
			Status:       sw.status,
			Action:       n.action,
			Summary:      n.summary,
			RemoteAddr:   host,
			ForwardedFor: clip(r.Header.Get("X-Forwarded-For"), 200),
			UserAgent:    clip(r.UserAgent(), 200),
			DurationMS:   int(time.Since(start).Milliseconds()),
		}
		if e.Status == 0 {
			e.Status = http.StatusOK
		}
		// The same line goes to the server log, so a log collector keeps a
		// copy that outlives the table's limit.
		slog.Info("audit", "actor", e.Actor, "on_behalf_of", e.OnBehalfOf, "method", e.Method, "path", e.Path,
			"status", e.Status, "action", e.Action, "summary", e.Summary, "remote_addr", e.RemoteAddr, "forwarded_for", e.ForwardedFor)
		if err := s.st.AddAudit(context.WithoutCancel(r.Context()), e); err != nil {
			slog.Error("store audit entry", "err", err)
		}
	})
}

func onBehalfOf(r *http.Request) string {
	raw := r.Header.Get(OnBehalfOfHeader)
	if name, err := url.PathUnescape(raw); err == nil {
		return name
	}
	return raw
}

// clip cuts a caller-supplied value to a length that is safe to store and
// drops control characters, which have no place in a log line.
func clip(v string, limit int) string {
	v = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, v)
	if len(v) > limit {
		v = v[:limit]
	}
	return strings.ToValidUTF8(v, "")
}

// listAudit returns the newest audit entries first. ?limit= defaults to 100,
// at most 500; ?before=<id> pages back.
func (s *Server) listAudit(w http.ResponseWriter, r *http.Request) {
	limit, err := queryLimit(r, 100)
	if err != nil {
		fail(w, err)
		return
	}
	before := int64(0)
	if v := r.URL.Query().Get("before"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 1 {
			fail(w, invalid("before must be the id of an audit entry"))
			return
		}
		before = n
	}
	entries, err := s.st.ListAudit(r.Context(), limit, before)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, entries)
}
