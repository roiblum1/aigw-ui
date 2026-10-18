package api

import (
	"net/http"

	"aigw-ui/clients"
)

// clientFile serves a tool for the people who use the models: aigw-token,
// the installer, the omp extension, or the Claude Code plugin as a
// marketplace archive. They come from the hub so that a machine with no
// internet installs them; a new hub version brings new copies, so they are
// asked for again every time.
func (s *Server) clientFile(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	w.Header().Set("Cache-Control", "no-cache")
	if name == clients.PluginArchive {
		data, err := clients.Archive()
		if err != nil {
			fail(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/gzip")
		w.Write(data)
		return
	}
	f, data, ok := clients.Read(name)
	if !ok {
		writeError(w, http.StatusNotFound, "no such file")
		return
	}
	w.Header().Set("Content-Type", f.Type)
	w.Write(data)
}
