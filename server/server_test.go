package server

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"aunefyren/solstein/logger"
	"aunefyren/solstein/settings"

	"github.com/sirupsen/logrus"
)

// captureLog swaps logger.Log for one writing into a buffer at the given level.
func captureLog(t *testing.T, level logrus.Level) *strings.Builder {
	t.Helper()
	original := logger.Log
	t.Cleanup(func() { logger.Log = original })

	var output strings.Builder
	logger.Log = logrus.New()
	logger.Log.SetOutput(&output)
	logger.Log.SetLevel(level)
	return &output
}

func TestHealth(t *testing.T) {
	router := newRouter("v1.2.3")

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/health", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["status"] != "ok" || body["version"] != "v1.2.3" {
		t.Errorf("body = %v", body)
	}
}

func TestRequestLoggerLevels(t *testing.T) {
	cases := []struct {
		name      string
		path      string
		wantLevel string
	}{
		{name: "success logs at debug", path: "/api/health", wantLevel: "level=debug"},
		{name: "not found logs at warn", path: "/nope", wantLevel: "level=warning"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			output := captureLog(t, logrus.DebugLevel)
			router := newRouter("test")

			router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, c.path, nil))

			if !strings.Contains(output.String(), c.wantLevel) || !strings.Contains(output.String(), c.path) {
				t.Errorf("log = %q, want %s entry for %s", output.String(), c.wantLevel, c.path)
			}
		})
	}
}

func TestRunShutsDownOnCancel(t *testing.T) {
	captureLog(t, logrus.InfoLevel)

	// Grab a free port, then release it for the server to bind.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()

	srv := New(settings.Config{Port: port}, "test")
	srv.Addr = "127.0.0.1:" + strconv.Itoa(port)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, srv) }()

	healthURL := "http://" + srv.Addr + "/api/health"
	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, err := http.Get(healthURL)
		if err == nil {
			resp.Body.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("server never came up: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run returned %v, want nil after cancel", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

func TestRunReportsListenError(t *testing.T) {
	captureLog(t, logrus.InfoLevel)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	srv := New(settings.Config{Port: 1}, "test")
	srv.Addr = listener.Addr().String() // already taken

	if err := Run(context.Background(), srv); err == nil {
		t.Error("expected an error when the port is taken")
	}
}
