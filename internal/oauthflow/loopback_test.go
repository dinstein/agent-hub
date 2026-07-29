package oauthflow

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// hit performs a GET against the callback listener. Tests use it to play
// the browser.
func hit(t *testing.T, base, query string) {
	t.Helper()
	resp, err := http.Get(base + "?" + query) //nolint:noctx // test stand-in for a browser redirect
	if err != nil {
		t.Logf("callback GET: %v", err) // races with shutdown are fine
		return
	}
	_ = resp.Body.Close()
}

// TestLoopbackBindsRandomPortEachTime is the toolport lesson: a FIXED
// callback port lets an abandoned authorization's listener intercept the
// next flow's callback. Random ports make that structurally impossible.
func TestLoopbackBindsRandomPortEachTime(t *testing.T) {
	seen := map[int]bool{}
	for i := 0; i < 8; i++ {
		ln, err := Listen()
		if err != nil {
			t.Fatal(err)
		}
		if ln.Port() == 0 {
			t.Fatal("listener reports port 0")
		}
		if seen[ln.Port()] {
			t.Fatalf("port %d reused across authorizations", ln.Port())
		}
		seen[ln.Port()] = true
		if want := fmt.Sprintf("http://127.0.0.1:%d/callback", ln.Port()); ln.RedirectURI() != want {
			t.Fatalf("redirect uri = %q want %q", ln.RedirectURI(), want)
		}
		defer func() { _ = ln.Close() }()
	}
}

// TestLoopbackRedirectURIUsesIPLiteral: RFC 8252 §8.3 requires 127.0.0.1,
// not "localhost" — the name can resolve to ::1 and then the registered
// URI and the browser's actual target disagree.
func TestLoopbackRedirectURIUsesIPLiteral(t *testing.T) {
	ln, err := Listen()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	if strings.Contains(ln.RedirectURI(), "localhost") {
		t.Fatalf("redirect uri must use the IP literal: %q", ln.RedirectURI())
	}
}

func TestLoopbackAcceptsMatchingCallback(t *testing.T) {
	ln, err := Listen()
	if err != nil {
		t.Fatal(err)
	}
	base := ln.RedirectURI()
	go func() {
		time.Sleep(20 * time.Millisecond)
		hit(t, base, "code=the-code&state=st-1")
	}()
	code, err := ln.Wait(context.Background(), "st-1", 5*time.Second)
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if code != "the-code" {
		t.Fatalf("code = %q", code)
	}
}

// TestLoopbackIgnoresStrayRequests: favicon fetches and browser probes hit
// loopback ports constantly. They must not end the flow.
func TestLoopbackIgnoresStrayRequests(t *testing.T) {
	ln, err := Listen()
	if err != nil {
		t.Fatal(err)
	}
	base := ln.RedirectURI()
	root := "http://127.0.0.1:" + fmt.Sprint(ln.Port())
	go func() {
		time.Sleep(20 * time.Millisecond)
		hit(t, root+"/favicon.ico", "")
		hit(t, root+"/", "")
		hit(t, base, "state=st-1") // state but no code: still stray
		hit(t, base, "nonsense=1")
		hit(t, base, "code=real&state=st-1")
	}()
	code, err := ln.Wait(context.Background(), "st-1", 5*time.Second)
	if err != nil {
		t.Fatalf("stray requests ended the flow: %v", err)
	}
	if code != "real" {
		t.Fatalf("code = %q", code)
	}
}

// TestLoopbackRejectsStateMismatch: a code WITH a wrong state is loud, not
// ignored — with a random port there is no benign explanation for it.
func TestLoopbackRejectsStateMismatch(t *testing.T) {
	for _, q := range []string{"code=c&state=wrong", "code=c"} {
		t.Run(q, func(t *testing.T) {
			ln, err := Listen()
			if err != nil {
				t.Fatal(err)
			}
			base := ln.RedirectURI()
			go func() {
				time.Sleep(20 * time.Millisecond)
				hit(t, base, q)
			}()
			_, err = ln.Wait(context.Background(), "st-1", 5*time.Second)
			if !errors.Is(err, ErrStateMismatch) {
				t.Fatalf("err = %v, want ErrStateMismatch", err)
			}
		})
	}
}

func TestLoopbackSurfacesAuthorizationServerError(t *testing.T) {
	ln, err := Listen()
	if err != nil {
		t.Fatal(err)
	}
	base := ln.RedirectURI()
	go func() {
		time.Sleep(20 * time.Millisecond)
		hit(t, base, "error=access_denied&error_description=nope&state=st-1")
	}()
	_, err = ln.Wait(context.Background(), "st-1", 5*time.Second)
	if !errors.Is(err, ErrAuthorizationDenied) {
		t.Fatalf("err = %v, want ErrAuthorizationDenied", err)
	}
}

func TestLoopbackTimesOut(t *testing.T) {
	ln, err := Listen()
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, err = ln.Wait(context.Background(), "st-1", 80*time.Millisecond)
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("timeout took %s", elapsed)
	}
	if LoopbackTimeout != 180*time.Second {
		t.Fatalf("the design fixes the total budget at 180s, got %s", LoopbackTimeout)
	}
}

// TestLoopbackReleasesPortAfterWait: the listener must be gone once Wait
// returns, or the next flow inherits a stale interceptor — the exact bug
// random ports exist to avoid.
func TestLoopbackReleasesPortAfterWait(t *testing.T) {
	ln, err := Listen()
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Port()
	_, _ = ln.Wait(context.Background(), "st", 30*time.Millisecond)
	again, err := ListenOnPort(port)
	if err != nil {
		t.Fatalf("port %d was not released: %v", port, err)
	}
	_ = again.Close()
}

// TestLoopbackBindsBeforeOpeningBrowser is the ordering invariant: by the
// time Open is called the port is already listening, so a redirect that
// arrives instantly cannot be refused.
func TestLoopbackBindsBeforeOpeningBrowser(t *testing.T) {
	var mu sync.Mutex
	var reachable bool
	flow := &LoopbackFlow{
		Timeout: 5 * time.Second,
		Open: func(raw string) error {
			u, err := url.Parse(raw)
			if err != nil {
				return err
			}
			redirect := u.Query().Get("redirect_uri")
			// Probe the callback port synchronously, before returning:
			// this is the instant-redirect race.
			resp, err := http.Get(redirect + "?probe=1") //nolint:noctx // browser stand-in
			mu.Lock()
			reachable = err == nil
			mu.Unlock()
			if err == nil {
				_ = resp.Body.Close()
			}
			go hit(t, redirect, "code=fast&state="+u.Query().Get("state"))
			return nil
		},
	}
	pkce, _ := NewPKCE()
	res, err := flow.Run(context.Background(), "st-1", func(redirectURI string) (string, error) {
		return BuildAuthorizeURL(AuthorizeRequest{
			Metadata:    &AuthServerMetadata{AuthorizationEndpoint: "https://as/authorize"},
			ClientID:    "c",
			RedirectURI: redirectURI,
			State:       "st-1",
			PKCE:        pkce,
		})
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !reachable {
		t.Fatal("the callback port was not listening when the browser opened")
	}
	if res.Code != "fast" {
		t.Fatalf("code = %q", res.Code)
	}
	if res.Port == 0 || CallbackPortOf(res.RedirectURI) != res.Port {
		t.Fatalf("port bookkeeping: %+v", res)
	}
}

// TestLoopbackReleasesPortWhenBrowserFails: a failed browser open must not
// leak the listener, or the manual-mode downgrade inherits a stray socket.
func TestLoopbackReleasesPortWhenBrowserFails(t *testing.T) {
	var port int
	flow := &LoopbackFlow{Open: func(string) error { return errors.New("no DISPLAY") }}
	_, err := flow.Run(context.Background(), "st", func(redirectURI string) (string, error) {
		port = CallbackPortOf(redirectURI)
		return "https://as/authorize", nil
	})
	if err == nil {
		t.Fatal("expected the browser failure to surface")
	}
	if !isBrowserOpenFailure(err) {
		t.Fatalf("browser failure not classified: %v", err)
	}
	again, err := ListenOnPort(port)
	if err != nil {
		t.Fatalf("port %d leaked after the browser failure: %v", port, err)
	}
	_ = again.Close()
}

func TestLoopbackBuildFailureReleasesPort(t *testing.T) {
	var port int
	flow := &LoopbackFlow{Open: NoBrowser}
	_, err := flow.Run(context.Background(), "st", func(redirectURI string) (string, error) {
		port = CallbackPortOf(redirectURI)
		return "", errors.New("registration failed")
	})
	if err == nil {
		t.Fatal("expected the build failure to surface")
	}
	again, err := ListenOnPort(port)
	if err != nil {
		t.Fatalf("port %d leaked: %v", port, err)
	}
	_ = again.Close()
}

func TestLoopbackWaitNeedsState(t *testing.T) {
	ln, err := Listen()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ln.Wait(context.Background(), "  ", time.Second); err == nil {
		t.Fatal("waiting without a state must be refused")
	}
}

func TestNoBrowserAlwaysFails(t *testing.T) {
	if err := NoBrowser("https://as/authorize"); err == nil {
		t.Fatal("NoBrowser must always fail so --no-browser takes the manual path")
	}
}

// TestParseLoopbackRedirectURI covers the pinned-redirect parser. Its
// strictness is a security boundary: this value decides where a browser
// delivers an authorization code.
func TestParseLoopbackRedirectURI(t *testing.T) {
	t.Run("accepts", func(t *testing.T) {
		cases := []struct {
			raw, host, path string
			port            int
		}{
			// The case this flag exists for: Google's allowlist spelling.
			{"http://localhost:8040/oauth2/callback", "localhost", "/oauth2/callback", 8040},
			{"http://127.0.0.1:8040/callback", "127.0.0.1", "/callback", 8040},
			{"http://[::1]:9000/cb", "::1", "/cb", 9000},
			// A loopback literal outside .0.1 is still loopback.
			{"http://127.0.0.2:1234/callback", "127.0.0.2", "/callback", 1234},
			// No path means the default, not an empty one.
			{"http://localhost:8040", "localhost", DefaultCallbackPath, 8040},
		}
		for _, tc := range cases {
			host, port, path, err := ParseLoopbackRedirectURI(tc.raw)
			if err != nil {
				t.Errorf("%s: %v", tc.raw, err)
				continue
			}
			if host != tc.host || port != tc.port || path != tc.path {
				t.Errorf("%s: got (%q,%d,%q), want (%q,%d,%q)",
					tc.raw, host, port, path, tc.host, tc.port, tc.path)
			}
		}
	})

	t.Run("rejects", func(t *testing.T) {
		for _, raw := range []string{
			"https://localhost:8040/cb",     // a loopback listener cannot serve TLS
			"http://example.com:8040/cb",    // would hand the code to another host
			"http://192.168.1.5:8040/cb",    // private, but still not this machine
			"http://localhost/cb",           // no port: cannot match an allowlist
			"http://localhost:0/cb",         // port 0 is OS-assigned, same problem
			"http://localhost:8040/cb?x=1",  // query would be silently dropped
			"http://localhost:8040/cb#frag", // ditto for a fragment
			"://bad",                        // unparseable
		} {
			if _, _, _, err := ParseLoopbackRedirectURI(raw); err == nil {
				t.Errorf("%s was accepted; it must be rejected", raw)
			}
		}
	})
}

// TestLoopbackRedirectURIHonorsPinnedHost pins that an explicit host reaches
// the advertised URI while the socket still binds 127.0.0.1 — the override
// changes what we ASK for, never where we listen.
func TestLoopbackRedirectURIHonorsPinnedHost(t *testing.T) {
	ln, err := listenLoopback(0, "localhost", "/oauth2/callback")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()

	want := "http://localhost:" + strconv.Itoa(ln.Port()) + "/oauth2/callback"
	if got := ln.RedirectURI(); got != want {
		t.Errorf("RedirectURI() = %q, want %q", got, want)
	}
	addr, ok := ln.ln.Addr().(*net.TCPAddr)
	if !ok || !addr.IP.IsLoopback() {
		t.Fatalf("listener bound %v, want a loopback address", ln.ln.Addr())
	}
	if addr.IP.String() != "127.0.0.1" {
		t.Errorf("listener bound %s, want 127.0.0.1 regardless of the advertised host", addr.IP)
	}

	// Default host is still the RFC 8252 literal.
	def, err := ListenOnPort(0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = def.Close() }()
	if !strings.HasPrefix(def.RedirectURI(), "http://127.0.0.1:") {
		t.Errorf("default RedirectURI() = %q, want the 127.0.0.1 literal", def.RedirectURI())
	}
}

// TestCallbackPageStatic pins every callback page as scriptless.
//
// Success used to run a countdown and call window.close(). Browsers refuse
// that for a tab the script did not open — which the OAuth tab never is —
// so the timer only delayed the "you can close it now" line it was already
// going to fall back to. The page now says so immediately. Reintroducing a
// self-close means reintroducing a promise the browser overrides.
func TestCallbackPageStatic(t *testing.T) {
	rec := httptest.NewRecorder()
	writeCallbackPage(rec, http.StatusOK, "Authorized",
		"agenthub received the authorization code. You can close this tab and return to the terminal.")
	ok := rec.Body.String()
	if strings.Contains(ok, "<script") || strings.Contains(ok, "window.close()") {
		t.Error("success page ships a script; browsers veto window.close() on an OAuth tab")
	}
	if strings.Contains(ok, "setInterval") || strings.Contains(ok, "Closing this tab") {
		t.Error("success page still counts down")
	}
	// The page must not promise a close it cannot deliver.
	if strings.Contains(ok, "closes automatically") {
		t.Error("success page claims it closes automatically")
	}
	// It must still tell the user the tab is theirs to close.
	if !strings.Contains(ok, "close this tab") {
		t.Errorf("success page does not tell the user they can close the tab: %s", ok)
	}

	rec = httptest.NewRecorder()
	writeCallbackPage(rec, http.StatusBadRequest, "Authorization failed", "denied")
	bad := rec.Body.String()
	if strings.Contains(bad, "window.close()") || strings.Contains(bad, "<script") {
		t.Error("failure page must not close itself")
	}

	for _, body := range []string{ok, bad} {
		if !strings.HasSuffix(body, "</body>") {
			t.Errorf("page is not closed properly: %q", body[max(0, len(body)-40):])
		}
	}
}
