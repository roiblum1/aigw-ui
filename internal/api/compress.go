package api

import (
	"compress/gzip"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

// minCompress is the smallest body worth compressing, when its length is
// known before it is written: below it the gzip header costs about what it
// saves.
const minCompress = 1024

var gzipWriters = sync.Pool{New: func() any {
	w, _ := gzip.NewWriterLevel(io.Discard, gzip.DefaultCompression)
	return w
}}

// compress gzips the answers of a client that takes gzip, when they are of a
// kind that shrinks: JSON, the pages, scripts and styles. With 1000 tenants
// the tenants list is 330 KB of JSON and about 30 KB gzipped, and the UI's
// script 440 KB and 130 KB.
func compress(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Vary", "Accept-Encoding")
		// A range is of the file as it is stored, not of a gzipped one.
		if !acceptsGzip(r) || r.Method == http.MethodHead || r.Header.Get("Range") != "" {
			next.ServeHTTP(w, r)
			return
		}
		cw := &compressWriter{ResponseWriter: w}
		defer cw.close()
		next.ServeHTTP(cw, r)
	})
}

// acceptsGzip reports whether the request's Accept-Encoding takes gzip.
func acceptsGzip(r *http.Request) bool {
	for _, part := range strings.Split(r.Header.Get("Accept-Encoding"), ",") {
		name, params, _ := strings.Cut(strings.TrimSpace(part), ";")
		if !strings.EqualFold(strings.TrimSpace(name), "gzip") {
			continue
		}
		q, ok := strings.CutPrefix(strings.ReplaceAll(params, " ", ""), "q=")
		if !ok {
			return true
		}
		v, err := strconv.ParseFloat(q, 64)
		return err == nil && v > 0
	}
	return false
}

// compressible reports whether a body of the content type shrinks enough to
// be worth gzipping.
func compressible(contentType string) bool {
	t, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return false
	}
	switch {
	case strings.HasPrefix(t, "text/"):
		return true
	case t == "application/json", t == "application/javascript", t == "image/svg+xml", t == "application/manifest+json":
		return true
	}
	return false
}

// compressWriter decides at the first write whether to gzip: by then the
// handler has set the status and the headers.
type compressWriter struct {
	http.ResponseWriter
	gz      *gzip.Writer
	decided bool
}

func (c *compressWriter) WriteHeader(status int) {
	if !c.decided {
		c.decide(status)
	}
	c.ResponseWriter.WriteHeader(status)
}

func (c *compressWriter) Write(p []byte) (int, error) {
	if !c.decided {
		c.WriteHeader(http.StatusOK)
	}
	if c.gz != nil {
		return c.gz.Write(p)
	}
	return c.ResponseWriter.Write(p)
}

func (c *compressWriter) decide(status int) {
	c.decided = true
	h := c.Header()
	// An answer with a secret in it is kept as it is: compressed next to text
	// a client chose, its length would tell something about the secret
	// (BREACH). Those answers are the ones that may not be cached.
	if status < 200 || status == http.StatusNoContent || status == http.StatusNotModified || status == http.StatusPartialContent ||
		h.Get("Content-Encoding") != "" || !compressible(h.Get("Content-Type")) || strings.Contains(h.Get("Cache-Control"), "no-store") {
		return
	}
	if n, err := strconv.Atoi(h.Get("Content-Length")); err == nil && n < minCompress {
		return
	}
	h.Del("Content-Length")
	h.Set("Content-Encoding", "gzip")
	gz := gzipWriters.Get().(*gzip.Writer)
	gz.Reset(c.ResponseWriter)
	c.gz = gz
}

// Flush sends what was written so far, compressed, for a handler that
// streams.
func (c *compressWriter) Flush() {
	if c.gz != nil {
		c.gz.Flush()
	}
	if f, ok := c.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets http.ResponseController reach the connection's own writer.
func (c *compressWriter) Unwrap() http.ResponseWriter { return c.ResponseWriter }

func (c *compressWriter) close() {
	if c.gz == nil {
		return
	}
	c.gz.Close()
	c.gz.Reset(io.Discard)
	gzipWriters.Put(c.gz)
	c.gz = nil
}
