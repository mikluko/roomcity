// Package server is the runtime's server: the listeners and the handlers
// mounted on them.
package server

import "net/http"

// Server collects the handlers of the server's three listeners. What a
// handler trusts of its caller decides which it is mounted on.
type Server struct {
	network, runtime, page *http.ServeMux
}

// New returns a Server with no handler.
func New() *Server {
	return &Server{network: http.NewServeMux(), runtime: http.NewServeMux(), page: http.NewServeMux()}
}

// Handle mounts h at path on the network listener, which the session pods
// and the homeserver reach: a handler there checks who calls. It is
// http.ServeMux's Handle; the pair a generated connect New...Handler returns
// goes in as it is. It panics on a path taken twice.
func (s *Server) Handle(path string, h http.Handler) { s.network.Handle(path, h) }

// HandleRuntime mounts h at path on the runtime's listener, a loopback port
// that only gc's supervisor, in the same pod, reaches. A handler there
// trusts its caller.
func (s *Server) HandleRuntime(path string, h http.Handler) { s.runtime.Handle(path, h) }

// HandlePage mounts h at path on the page listener, a loopback port that
// the authenticating proxy of the same pod takes whole as its upstream.
func (s *Server) HandlePage(path string, h http.Handler) { s.page.Handle(path, h) }

// Handler returns the handler of the network listener.
func (s *Server) Handler() http.Handler { return s.network }

// RuntimeHandler returns the handler of the runtime's listener.
func (s *Server) RuntimeHandler() http.Handler { return s.runtime }

// PageHandler returns the handler of the page listener.
func (s *Server) PageHandler() http.Handler { return s.page }
