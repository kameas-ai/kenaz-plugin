package daemon

import (
	"errors"
	"io"
	"net"
	"os"
	"strconv"
	"sync"
	"syscall"
	"testing"
	"time"
)

func hasIPv6Loopback() bool {
	l, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		return false
	}
	l.Close()
	return true
}

// Another local process holding [::1]:P must not be able to answer for
// "localhost:P" while the relay holds only 127.0.0.1:P: that port is
// skipped.
func TestListenSkipsPortSquattedOnIPv6Loopback(t *testing.T) {
	if !hasIPv6Loopback() {
		t.Skip("no IPv6 loopback in this environment")
	}
	squat, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Fatal(err)
	}
	defer squat.Close()
	p := squat.Addr().(*net.TCPAddr).Port
	ln, err := listen([]int{p, 0})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	got := ln.Addr().(*net.TCPAddr).Port
	if got == p {
		t.Fatalf("bound port %d although [::1]:%d is held by another process", p, p)
	}
	// Both loopbacks reach the relay.
	for _, host := range []string{"127.0.0.1", "::1"} {
		go func() {
			if c, err := ln.Accept(); err == nil {
				c.Write([]byte("relay"))
				c.Close()
			}
		}()
		c, err := net.Dial("tcp", net.JoinHostPort(host, strconv.Itoa(got)))
		if err != nil {
			t.Fatalf("%s: %v", host, err)
		}
		b, _ := io.ReadAll(c)
		c.Close()
		if string(b) != "relay" {
			t.Fatalf("%s answered %q", host, b)
		}
	}
}

// The bind policy without a real IPv6 stack: a taken [::1] side skips the
// port, a missing IPv6 loopback falls back to 127.0.0.1 only.
func TestListenBindPolicy(t *testing.T) {
	defer func(f func(string, string) (net.Listener, error)) { listenTCP = f }(listenTCP)
	v6err := error(&net.OpError{Op: "listen", Err: syscall.EADDRINUSE})
	var v6calls []string
	listenTCP = func(network, addr string) (net.Listener, error) {
		if network == "tcp6" {
			v6calls = append(v6calls, addr)
			return nil, v6err
		}
		return net.Listen("tcp4", "127.0.0.1:0")
	}
	if _, err := listen([]int{4318}); err == nil {
		t.Fatal("bound although [::1] was taken")
	}
	if len(v6calls) != 1 {
		t.Fatalf("v6 attempts %v", v6calls)
	}
	v6err = &net.OpError{Op: "listen", Err: syscall.EADDRNOTAVAIL}
	ln, err := listen([]int{4318})
	if err != nil {
		t.Fatalf("no IPv6 loopback: %v", err)
	}
	ln.Close()
	if !noIPv6Loopback(v6err) || noIPv6Loopback(errors.New("x")) {
		t.Fatal("classification")
	}
}

// flakyListener fails Accept with transient errors before delegating.
type flakyListener struct {
	net.Listener
	mu    sync.Mutex
	fails int
}

func (f *flakyListener) Accept() (net.Conn, error) {
	f.mu.Lock()
	if f.fails > 0 {
		f.fails--
		f.mu.Unlock()
		return nil, &net.OpError{Op: "accept", Net: "tcp", Err: os.NewSyscallError("accept", syscall.EMFILE)}
	}
	f.mu.Unlock()
	return f.Listener.Accept()
}

// A transient accept error (out of file descriptors) is retried with
// backoff; the listeners keep serving instead of all closing.
func TestMultiListenerRetriesTransientAcceptErrors(t *testing.T) {
	inner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	fl := &flakyListener{Listener: inner, fails: 3}
	m := newMultiListener(fl)
	defer m.Close()
	go func() {
		c, err := net.Dial("tcp", inner.Addr().String())
		if err == nil {
			c.Close()
		}
	}()
	done := make(chan error, 1)
	go func() {
		c, err := m.Accept()
		if err == nil {
			c.Close()
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("accept after transient errors: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no connection accepted")
	}
	if !retryableAccept(&net.OpError{Err: os.NewSyscallError("accept", syscall.EMFILE)}) || retryableAccept(net.ErrClosed) {
		t.Fatal("classification")
	}
}
