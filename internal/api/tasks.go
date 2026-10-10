package api

import (
	"context"
	"log/slog"
	"net/http"
)

// changed records a change that has to reach the clusters: in the task log,
// which queues the sync, and in the audit log.
func (s *Server) changed(r *http.Request, action, summary string) {
	note(r, action, summary)
	s.sy.Changed(r.Context(), action, summary)
}

// record adds a task for a sync the handler runs itself. clusterID limits it
// to one cluster when not empty.
func (s *Server) record(r *http.Request, action, summary, clusterID string) {
	note(r, action, summary)
	s.sy.Record(r.Context(), action, summary, clusterID)
}

// log adds an entry to the task log for something that is already finished
// and needs no sync.
func (s *Server) log(r *http.Request, action, summary string, ok bool, message string) {
	note(r, action, summary)
	if err := s.st.CreateFinishedTask(context.WithoutCancel(r.Context()), action, summary, ok, message); err != nil {
		slog.Error("record task", "err", err)
	}
}

// listTasks returns the newest tasks first. ?limit= defaults to 50, at most 500.
func (s *Server) listTasks(w http.ResponseWriter, r *http.Request) {
	listLimited(w, r, 50, s.st.ListTasks)
}
