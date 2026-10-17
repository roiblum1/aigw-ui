package api

import (
	"log/slog"
	"net/http"
	"strconv"
	"time"
)

// slowRequest is how long an admin request may take in all before the log
// says so.
const slowRequest = 2 * time.Second

// timed says how long the hub took to answer an admin request: in the
// Server-Timing header ("app", milliseconds until the answer starts), so a
// client tells the hub's time from its network's, and in the log when the
// whole request took longer than slowRequest. The test machine's requests to
// the sandbox hub took 0.53 s for /healthz alone, and its timings of the
// tenants and usage lists (1.4 and 2.2 to 2.9 s at 1,000 tenants) were the
// link's: the hub took 5 and 12 ms for them locally (2026-10-17).
func timed(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tw := &timingWriter{ResponseWriter: w, start: time.Now()}
		next.ServeHTTP(tw, r)
		if took := time.Since(tw.start); took > slowRequest {
			slog.Warn("slow request", "method", r.Method, "path", r.URL.Path, "status", tw.status, "took_ms", took.Milliseconds())
		}
	})
}

// timingWriter sets Server-Timing when the handler starts its answer.
type timingWriter struct {
	http.ResponseWriter
	start  time.Time
	status int
}

func (w *timingWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
		ms := float64(time.Since(w.start).Microseconds()) / 1000
		w.Header().Set("Server-Timing", "app;dur="+strconv.FormatFloat(ms, 'f', 1, 64))
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *timingWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(b)
}

// Flush passes a streaming handler's flush on.
func (w *timingWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets http.ResponseController reach the writer below.
func (w *timingWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
