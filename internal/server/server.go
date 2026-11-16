// Package server is the runtime's server: the listeners and the handlers
// mounted on them.
package server

import "net/http"

// Server collects the handlers of the server's network listener, the one
// sessions and the homeserver reach.
type Server struct {
	mux *http.ServeMux
}

// New returns a Server with no handler.
func New() *Server {
	return &Server{mux: http.NewServeMux()}
}

// Handle mounts h at path, as http.ServeMux does; the pair a generated
// connect New...Handler returns goes in as it is. It panics on a path taken
// twice.
func (s *Server) Handle(path string, h http.Handler) {
	s.mux.Handle(path, h)
}

// Handler returns the handler of every mounted path.
func (s *Server) Handler() http.Handler {
	return s.mux
}
