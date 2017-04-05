package server

import (
	"context"
	"errors"
	"sync"

	"github.com/10gen/mongo-go-driver/conn"
	"github.com/10gen/mongo-go-driver/model"
	"github.com/10gen/mongo-go-driver/msg"
)

// ErrServerClosed occurs when an attempt to get a connection is made
// after the server has been closed.
var ErrServerClosed = errors.New("server is closed")

// New creates a new server. Internally, it
// creates a new Monitor with which to monitor the
// state of the server. When the Server is closed,
// the monitor will be stopped.
func New(addr model.Addr, opts ...Option) (*Server, error) {
	monitor, err := StartMonitor(addr, opts...)
	if err != nil {
		return nil, err
	}

	s := NewWithMonitor(monitor, opts...)
	s.ownsMonitor = true
	return s, nil
}

// NewWithMonitor creates a new Server from
// an existing monitor. When the server is closed,
// the monitor will not be stopped. Any unspecified
// options will have their default value pulled from the monitor.
// Any monitor specific options will be ignored.
func NewWithMonitor(monitor *Monitor, opts ...Option) *Server {
	cfg := monitor.cfg.reconfig(opts...)
	server := &Server{
		monitor: monitor,
	}

	server.conns = conn.NewPool(
		uint64(cfg.maxIdleConns),
		conn.OpeningProvider(cfg.opener, monitor.addr, cfg.connOpts...),
	)
	server.connProvider = server.conns.Get

	if cfg.maxConns != 0 {
		server.connProvider = conn.CappedProvider(uint64(cfg.maxConns), server.connProvider)
	}

	updates, cancel, _ := monitor.Subscribe()
	server.cancelSubscription = cancel
	go func() {
		for desc := range updates {
			server.applyUpdate(desc)
		}
	}()

	return server
}

// Server is a logical connection to a server.
type Server struct {
	lock sync.Mutex // protects monitor and conns

	monitor      *Monitor
	conns        conn.Pool
	connProvider conn.Provider

	cancelSubscription func()
	ownsMonitor        bool

	hasCurrent  bool
	current     *model.Server
	currentLock sync.Mutex
}

// Close closes the server.
func (s *Server) Close() {
	s.lock.Lock()
	defer s.lock.Unlock()
	if s.monitor == nil {
		// already closed
		return
	}

	s.cancelSubscription()
	s.conns.Close()
	if s.ownsMonitor {
		s.monitor.Stop()
	}

	s.conns = nil
	s.monitor = nil
	s.connProvider = nil
}

// Connection gets a connection to the server.
func (s *Server) Connection(ctx context.Context) (conn.Connection, error) {
	s.lock.Lock()
	p := s.connProvider
	s.lock.Unlock()

	if p == nil {
		return nil, ErrServerClosed
	}

	c, err := p(ctx)
	if err != nil {
		return nil, err
	}

	return &serverConn{
		Connection: c,
		server:     s,
	}, nil
}

// Model gets a description of the server as of the last heartbeat.
func (s *Server) Model() *model.Server {
	s.currentLock.Lock()
	current := s.current
	s.currentLock.Unlock()
	return current
}

func (s *Server) applyUpdate(m *model.Server) {
	var first bool
	s.currentLock.Lock()
	s.current = m
	first = !s.hasCurrent
	s.hasCurrent = true
	s.currentLock.Unlock()

	if first {
		// don't clear the pool for the first update.
		return
	}

	switch m.Kind {
	case model.Unknown:
		s.lock.Lock()
		conns := s.conns
		s.lock.Unlock()

		if conns != nil {
			conns.Clear()
		}
	}
}

type serverConn struct {
	conn.Connection
	server *Server
}

// Read reads a message from the connection.
func (c *serverConn) Read(ctx context.Context, responseTo int32) (msg.Response, error) {
	resp, err := c.Connection.Read(ctx, responseTo)
	if err != nil {
		c.server.monitor.RequestImmediateCheck()
	}
	return resp, err
}

// Write writes a number of messages to the connection.
func (c *serverConn) Write(ctx context.Context, reqs ...msg.Request) error {
	err := c.Connection.Write(ctx, reqs...)
	if err != nil {
		c.server.monitor.RequestImmediateCheck()
	}

	return err
}