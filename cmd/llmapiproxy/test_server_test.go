package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

func newTestServer(t *testing.T, handler http.Handler) *httptest.Server {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("skipping test: unable to bind loopback listener in this environment: %v", err)
	}
	ts := httptest.NewUnstartedServer(handler)
	ts.Listener = listener
	ts.Start()
	return ts
}
