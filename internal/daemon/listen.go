package daemon

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"sync"
	"syscall"
	"time"
)

// listenTCP is net.Listen (a test seam).
var listenTCP = net.Listen

// listen binds the first available preferred port on both loopbacks:
// 127.0.0.1 and, where the host has IPv6 loopback, [::1]. Browsers resolve
// "localhost" (the approval page's origin) to either, so the relay must own
// the port on both; a port whose [::1] side another process holds is
// skipped (fail closed) rather than letting that process answer as the
// relay.
func listen(ports []int) (net.Listener, error) {
	var errs []error
	for _, p := range ports {
		tries := 1
		if p == 0 {
			tries = 5
		}
		for i := 0; i < tries; i++ {
			v4, err := listenTCP("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(p)))
			if err != nil {
				errs = append(errs, err)
				break
			}
			port := v4.Addr().(*net.TCPAddr).Port
			v6, err := listenTCP("tcp6", net.JoinHostPort("::1", strconv.Itoa(port)))
			if err != nil {
				if noIPv6Loopback(err) {
					return v4, nil
				}
				v4.Close()
				errs = append(errs, fmt.Errorf("[::1]:%d: %w", port, err))
				continue
			}
			return newMultiListener(v4, v6), nil
		}
	}
	return nil, fmt.Errorf("no loopback port available: %w", errors.Join(errs...))
}

// noIPv6Loopback reports whether err means the host has no IPv6 loopback
// (as opposed to the address being taken).
func noIPv6Loopback(err error) bool {
	for _, e := range []error{syscall.EADDRNOTAVAIL, syscall.EAFNOSUPPORT, syscall.EPROTONOSUPPORT,
		syscall.Errno(10047), syscall.Errno(10049)} { // WSAEAFNOSUPPORT, WSAEADDRNOTAVAIL
		if errors.Is(err, e) {
			return true
		}
	}
	return false
}

// multiListener accepts from several listeners; Addr is the first one's.
type multiListener struct {
	ls    []net.Listener
	conns chan net.Conn
	done  chan struct{}
	once  sync.Once
}

func newMultiListener(ls ...net.Listener) *multiListener {
	m := &multiListener{ls: ls, conns: make(chan net.Conn), done: make(chan struct{})}
	for _, l := range ls {
		go m.loop(l)
	}
	return m
}

func (m *multiListener) loop(l net.Listener) {
	var delay time.Duration
	for {
		c, err := l.Accept()
		if err != nil {
			if !retryableAccept(err) {
				m.Close()
				return
			}
			// Out of file descriptors, an aborted connection and the like:
			// back off and keep serving, as net/http does (5 ms doubling to
			// 1 s), instead of closing every listener.
			if delay == 0 {
				delay = 5 * time.Millisecond
			} else if delay *= 2; delay > time.Second {
				delay = time.Second
			}
			t := time.NewTimer(delay)
			select {
			case <-t.C:
				continue
			case <-m.done:
				t.Stop()
				return
			}
		}
		delay = 0
		select {
		case m.conns <- c:
		case <-m.done:
			c.Close()
			return
		}
	}
}

// retryableAccept reports whether an Accept error is transient (the
// listener itself is still usable).
func retryableAccept(err error) bool {
	if errors.Is(err, net.ErrClosed) {
		return false
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	for _, e := range []error{syscall.EMFILE, syscall.ENFILE, syscall.ENOBUFS, syscall.ENOMEM, syscall.ECONNABORTED, syscall.ECONNRESET, syscall.EINTR} {
		if errors.Is(err, e) {
			return true
		}
	}
	return false
}

func (m *multiListener) Accept() (net.Conn, error) {
	select {
	case c := <-m.conns:
		return c, nil
	case <-m.done:
		return nil, net.ErrClosed
	}
}

func (m *multiListener) Close() error {
	var errs []error
	m.once.Do(func() {
		close(m.done)
		for _, l := range m.ls {
			errs = append(errs, l.Close())
		}
	})
	return errors.Join(errs...)
}

func (m *multiListener) Addr() net.Addr { return m.ls[0].Addr() }
