package api

import (
	"net/http"

	"aigw-ui/docs"
	"aigw-ui/internal/store"
)

// platformArchitecture returns the platform design page. It sits behind the
// admin token like the rest of the API, because it names the sites.
func (s *Server) platformArchitecture(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// The UI shows the page in a frame without script rights; this says the
	// same to a browser that opens the address directly.
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'")
	w.Write(docs.PlatformArchitecture)
}

// listDocs returns the documents of the Docs page: the guides and the
// release notes that are built into the server.
func (s *Server) listDocs(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, docs.List())
}

// readDoc returns one document as Markdown. Only a name from the list is
// read, so the path in the request never reaches the file system.
func (s *Server) readDoc(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	for _, d := range docs.List() {
		if d.Name != name {
			continue
		}
		text, err := docs.Read(name)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"name": d.Name, "title": d.Title, "markdown": text})
		return
	}
	fail(w, store.ErrNotFound)
}
