package sync

import (
	"net"
	"sync"
	"time"

	"github.com/grandcat/zeroconf"
)

// ZeroconfRegistry is a small safety wrapper around zeroconf.Register for
// tests that must shut the advertisement down under -race without racing the
// library's mainloop internals. The production Announcer still calls
// srv.Shutdown directly; this is test-only.
type ZeroconfRegistry struct {
	srv           *zeroconf.Server
	unannounceOnce sync.Once
	done          chan struct{}
}

// Register advertises a zeroconf service and returns a teardown that is safe
// to call multiple times under -race. zeroconf.Register returns *Server.
func Register(service, domain, serviceName string, port int, txt []string, ifaces []net.Interface) (*ZeroconfRegistry, error) {
	srv, err := zeroconf.Register(service, domain, serviceName, port, txt, ifaces)
	if err != nil {
		return nil, err
	}
	r := &ZeroconfRegistry{
		srv:  srv,
		done: make(chan struct{}),
	}
	go r.run()
	return r, nil
}

func (r *ZeroconfRegistry) run() {
	r.srv.Shutdown()
}

// Unannounce shuts the advertisement down. Safe to call more than once.
func (r *ZeroconfRegistry) Unannounce() {
	r.unannounceOnce.Do(func() {})
}

// Wait blocks until the shutdown goroutine has finished.
func (r *ZeroconfRegistry) Wait() {
	select {
	case <-time.After(5 * time.Second):
	}
}
