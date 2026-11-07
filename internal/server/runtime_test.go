package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestBothListenersStartAtomically(t *testing.T) {
	manager, _ := newManager(t, portsConfig(t, ""))
	rt, _ := startRuntime(t, Options{Source: manager, Proxy: okHandler(), Admin: AdminSurface(manager)})
	if response := get(t, rt.ProxyAddr(), "/x"); response.StatusCode != http.StatusNoContent {
		t.Fatalf("proxy status %d", response.StatusCode)
	}
	if response := get(t, rt.AdminAddr(), "/healthz"); response.StatusCode != http.StatusOK {
		t.Fatalf("admin status %d", response.StatusCode)
	}
}

func TestSecondBindFailureClosesFirst(t *testing.T) {
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = taken.Close() }()
	proxyPort := freePort(t)
	adminPort := taken.Addr().(*net.TCPAddr).Port
	manager, _ := newManager(t, fmt.Sprintf("listeners:\n  proxy: {port: %d}\n  admin: {port: %d}\n", proxyPort, adminPort))

	_, err = Open(context.Background(), Options{Source: manager, Proxy: okHandler(), Admin: okHandler()})
	if err == nil || !strings.Contains(err.Error(), "admin listener") {
		t.Fatalf("expected the admin bind to fail, got %v", err)
	}
	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", proxyPort))
	if err != nil {
		t.Fatalf("the proxy socket was left open: %v", err)
	}
	_ = listener.Close()
}

func TestWorkerFailureStopsRuntime(t *testing.T) {
	cases := map[string]Worker{
		"error":        func(context.Context) error { return errors.New("boom") },
		"early return": func(context.Context) error { return nil },
	}
	for name, failing := range cases {
		t.Run(name, func(t *testing.T) {
			manager, _ := newManager(t, portsConfig(t, ""))
			var healthyStopped atomic.Bool
			healthy := func(ctx context.Context) error {
				<-ctx.Done()
				healthyStopped.Store(true)
				return nil
			}
			rt, err := Open(context.Background(), Options{
				Source: manager, Proxy: okHandler(), Admin: okHandler(), Workers: []Worker{healthy, failing},
			})
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- rt.Run(context.Background()) }()
			select {
			case err := <-done:
				if !errors.Is(err, ErrComponentStopped) {
					t.Fatalf("got %v", err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("runtime kept running after a worker failure")
			}
			if !healthyStopped.Load() {
				t.Fatal("the other worker was not stopped")
			}
		})
	}
}

func TestShutdownDrainsThenCancels(t *testing.T) {
	t.Run("a request that finishes in time completes", func(t *testing.T) {
		manager, _ := newManager(t, portsConfig(t, ""))
		started := make(chan struct{})
		release := make(chan struct{})
		handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			close(started)
			<-release
			_, _ = w.Write([]byte("done"))
		})
		rt, stop := startRuntime(t, Options{Source: manager, Proxy: handler, Admin: okHandler(), DrainTimeout: 5 * time.Second})
		result := make(chan string, 1)
		go func() {
			response, err := http.Get("http://" + rt.ProxyAddr().String() + "/x")
			if err != nil {
				result <- err.Error()
				return
			}
			defer func() { _ = response.Body.Close() }()
			body, _ := io.ReadAll(response.Body)
			result <- string(body)
		}()
		<-started
		stopped := make(chan error, 1)
		go func() { stopped <- stop() }()
		time.Sleep(100 * time.Millisecond)
		close(release)
		if got := <-result; got != "done" {
			t.Fatalf("request lost during drain: %q", got)
		}
		if err := <-stopped; err != nil {
			t.Fatal(err)
		}
	})

	t.Run("a request past the deadline is canceled", func(t *testing.T) {
		manager, _ := newManager(t, portsConfig(t, ""))
		started := make(chan struct{})
		canceled := make(chan struct{})
		handler := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			close(started)
			<-r.Context().Done()
			close(canceled)
		})
		rt, stop := startRuntime(t, Options{Source: manager, Proxy: handler, Admin: okHandler(), DrainTimeout: 100 * time.Millisecond})
		go func() {
			if response, err := http.Get("http://" + rt.ProxyAddr().String() + "/x"); err == nil {
				_ = response.Body.Close()
			}
		}()
		<-started
		began := time.Now()
		if err := stop(); err != nil {
			t.Fatal(err)
		}
		select {
		case <-canceled:
		default:
			t.Fatal("the request was not canceled before stop returned")
		}
		if elapsed := time.Since(began); elapsed > 3*time.Second {
			t.Fatalf("shutdown took %s", elapsed)
		}
	})
}
