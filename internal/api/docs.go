package api

import (
	"net/http"

	"aigw-ui/docs"
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
