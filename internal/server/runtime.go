// Package server owns the two HTTP listeners and their lifecycle.
package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"
)

const (
	maxHeaderBytes      = 64 << 10
	defaultDrainTimeout = 10 * time.Second
	forcedStopTimeout   = 5 * time.Second
)

// ErrComponentStopped reports that a supervised worker or server ended on its own.
var ErrComponentStopped = errors.New("a supervised component stopped unexpectedly")

// Worker is a background component supervised with the servers. It must return
// when its context ends.
type Worker func(ctx context.Context) error

// Options configures a Runtime.
type Options struct {
	Source  SnapshotSource
	Proxy   http.Handler
	Admin   http.Handler
	Workers []Worker
	Logger  *slog.Logger
	// DrainTimeout bounds how long operations in flight may finish on shutdown.
	DrainTimeout time.Duration
}

// Runtime is a pair of listeners already bound and ready to serve.
type Runtime struct {
	options Options
	proxy   net.Listener
	admin   net.Listener
}

// Open binds both listeners, or none: a failure on the second closes the first, so
// the service is never partially available.
func Open(ctx context.Context, options Options) (*Runtime, error) {
	if options.Logger == nil {
		options.Logger = slog.New(slog.DiscardHandler)
	}
	if options.DrainTimeout <= 0 {
		options.DrainTimeout = defaultDrainTimeout
	}
	listeners := options.Source.Capture().Listeners()
	var config net.ListenConfig
	proxyAddr := net.JoinHostPort(listeners.Proxy.Bind, strconv.FormatInt(listeners.Proxy.Port, 10))
	proxyListener, err := config.Listen(ctx, "tcp", proxyAddr)
	if err != nil {
		return nil, fmt.Errorf("could not open the proxy listener on %s", proxyAddr)
	}
	adminAddr := net.JoinHostPort(listeners.Admin.Bind, strconv.FormatInt(listeners.Admin.Port, 10))
	adminListener, err := config.Listen(ctx, "tcp", adminAddr)
	if err != nil {
		_ = proxyListener.Close()
		return nil, fmt.Errorf("could not open the admin listener on %s", adminAddr)
	}
	return &Runtime{options: options, proxy: proxyListener, admin: adminListener}, nil
}

// ProxyAddr is the bound address of the proxy listener.
func (rt *Runtime) ProxyAddr() net.Addr { return rt.proxy.Addr() }

// AdminAddr is the bound address of the admin listener.
func (rt *Runtime) AdminAddr() net.Addr { return rt.admin.Addr() }

// Run serves until ctx ends or a component fails, then drains. Shutdown order is
// deliberate: stop accepting, let operations finish, cancel what remains, and only
// then stop the workers, which must outlive every request that depends on them.
func (rt *Runtime) Run(ctx context.Context) error {
	operations, cancelOperations := context.WithCancel(context.Background())
	defer cancelOperations()
	workers, cancelWorkers := context.WithCancel(context.Background())
	defer cancelWorkers()

	failed := make(chan struct{})
	var failOnce sync.Once
	fail := func() { failOnce.Do(func() { close(failed) }) }

	var supervised sync.WaitGroup
	for _, worker := range rt.options.Workers {
		supervised.Go(func() {
			if err := worker(workers); err != nil || workers.Err() == nil {
				fail()
			}
		})
	}

	var inFlight tracker
	proxyServer := newServer(operations, &inFlight, rt.options.Proxy)
	adminServer := newServer(operations, &inFlight, rt.options.Admin)
	var serving sync.WaitGroup
	serve := func(server *http.Server, listener net.Listener) {
		serving.Go(func() {
			admission := &admissionListener{Listener: listener, source: rt.options.Source}
			if err := server.Serve(admission); !errors.Is(err, http.ErrServerClosed) {
				fail()
			}
		})
	}
	serve(proxyServer, rt.proxy)
	serve(adminServer, rt.admin)
	rt.options.Logger.Info("listening", "proxy", rt.proxy.Addr().String(), "admin", rt.admin.Addr().String())

	var outcome error
	select {
	case <-ctx.Done():
	case <-failed:
		outcome = ErrComponentStopped
	}

	rt.drain(proxyServer, adminServer, cancelOperations, &inFlight)
	serving.Wait()
	cancelWorkers()
	supervised.Wait()
	return outcome
}

func newServer(operations context.Context, inFlight *tracker, handler http.Handler) *http.Server {
	server := &http.Server{
		Handler:                      inFlight.wrap(handler),
		MaxHeaderBytes:               maxHeaderBytes,
		BaseContext:                  func(net.Listener) context.Context { return operations },
		DisableGeneralOptionsHandler: true,
		// The default logger would receive raw client and network errors.
		ErrorLog: log.New(io.Discard, "", 0),
	}
	// One request per connection is what lets each request get the header deadline
	// of the configuration active when it arrived.
	server.SetKeepAlivesEnabled(false)
	return server
}

func (rt *Runtime) drain(proxyServer, adminServer *http.Server, cancelOperations context.CancelFunc, inFlight *tracker) {
	drainCtx, cancelDrain := context.WithTimeout(context.Background(), rt.options.DrainTimeout)
	defer cancelDrain()
	var stopping sync.WaitGroup
	for _, server := range []*http.Server{proxyServer, adminServer} {
		stopping.Go(func() {
			if err := server.Shutdown(drainCtx); err != nil {
				cancelOperations()
				_ = server.Close()
			}
		})
	}
	stopping.Wait()
	cancelOperations()
	if !inFlight.wait(forcedStopTimeout) {
		rt.options.Logger.Error("some requests did not stop in time")
	}
}

// tracker counts requests still inside a handler. Closing a server closes its
// sockets but does not wait for handlers.
type tracker struct {
	mu    sync.Mutex
	count int
	idle  chan struct{}
}

func (t *tracker) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.mu.Lock()
		t.count++
		t.mu.Unlock()
		defer func() {
			t.mu.Lock()
			defer t.mu.Unlock()
			t.count--
			if t.count == 0 && t.idle != nil {
				close(t.idle)
				t.idle = nil
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func (t *tracker) wait(limit time.Duration) bool {
	t.mu.Lock()
	if t.count == 0 {
		t.mu.Unlock()
		return true
	}
	idle := make(chan struct{})
	t.idle = idle
	t.mu.Unlock()
	timer := time.NewTimer(limit)
	defer timer.Stop()
	select {
	case <-idle:
		return true
	case <-timer.C:
		return false
	}
}
