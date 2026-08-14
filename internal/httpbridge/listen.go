package httpbridge

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ShutdownGrace bounds the drain phase of Serve.
const ShutdownGrace = 5 * time.Second

// Listen binds addr and returns the listeners to serve on.
//
// Dual-stack loopback (docs/architecture.md#the-processes): a client told to reach
// "localhost" may resolve it to either 127.0.0.1 or ::1, and which one it
// picks is out of our hands. Binding only one family produces the worst
// possible failure — it works on the developer's machine and refuses
// connections on the user's — so "localhost" binds BOTH.
//
// Port 0 is handled explicitly: the first listener's actual port is read
// back and reused for the second, or the two halves would land on different
// ports and only one of them would be the endpoint anybody was told about.
//
// Failure direction: if the second family cannot be bound (a host without
// IPv6, or with it disabled) the first one still serves and the error is
// returned as a warning, not a failure — refusing to start because ::1 is
// unavailable would be a regression against a single-stack machine. Both
// failing is a hard error.
func Listen(addr string) (listeners []net.Listener, warn error, err error) {
	host, port, serr := net.SplitHostPort(addr)
	if serr != nil {
		return nil, nil, fmt.Errorf("httpbridge: parsing listen address %q: %w", addr, serr)
	}
	if !strings.EqualFold(strings.TrimSpace(host), "localhost") {
		l, lerr := net.Listen("tcp", addr)
		if lerr != nil {
			return nil, nil, fmt.Errorf("httpbridge: binding %s: %w", addr, lerr)
		}
		return []net.Listener{l}, nil, nil
	}

	first, ferr := net.Listen("tcp", net.JoinHostPort("127.0.0.1", port))
	if ferr != nil {
		// IPv4 loopback unavailable: try IPv6 alone before giving up.
		second, serr2 := net.Listen("tcp", net.JoinHostPort("::1", port))
		if serr2 != nil {
			return nil, nil, fmt.Errorf("httpbridge: binding %s: %w", addr, errors.Join(ferr, serr2))
		}
		return []net.Listener{second}, ferr, nil
	}
	bound := port
	if p := portOf(first); p != "" {
		bound = p
	}
	second, serr2 := net.Listen("tcp", net.JoinHostPort("::1", bound))
	if serr2 != nil {
		return []net.Listener{first}, serr2, nil
	}
	return []net.Listener{first, second}, nil, nil
}

// portOf reads the concrete port a listener bound (resolving port 0).
func portOf(l net.Listener) string {
	ta, ok := l.Addr().(*net.TCPAddr)
	if !ok {
		return ""
	}
	return strconv.Itoa(ta.Port)
}

// Serve runs the server on every listener until ctx is done, then drains for
// ShutdownGrace and force-closes the stragglers. It closes every listener
// before returning, whichever path it takes.
func (s *Server) Serve(ctx context.Context, listeners ...net.Listener) error {
	if len(listeners) == 0 {
		return errors.New("httpbridge: Serve needs at least one listener")
	}
	hs := s.HTTPServer()
	errCh := make(chan error, len(listeners))
	var wg sync.WaitGroup
	for _, l := range listeners {
		wg.Add(1)
		go func(l net.Listener) {
			defer wg.Done()
			if err := hs.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errCh <- err
			}
		}(l)
	}

	select {
	case <-ctx.Done():
	case err := <-errCh:
		_ = hs.Close()
		wg.Wait()
		return err
	}
	shutCtx, cancel := context.WithTimeout(context.Background(), ShutdownGrace)
	defer cancel()
	if err := hs.Shutdown(shutCtx); err != nil {
		_ = hs.Close()
	}
	wg.Wait()
	return nil
}
