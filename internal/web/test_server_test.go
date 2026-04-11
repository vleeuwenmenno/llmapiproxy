package web

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

func newTestServer(t testing.TB, handler http.Handler) *httptest.Server {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("listener unavailable in this environment: %v", err)
	}

	srv := httptest.NewUnstartedServer(handler)
	srv.Listener = ln
	srv.Start()
	return srv
}
