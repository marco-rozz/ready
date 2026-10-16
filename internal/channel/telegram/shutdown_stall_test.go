package telegram

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/rs/zerolog"
)

// testToken is an arbitrary bot token used to shape the local test endpoints
// (/bot<token>/getMe, /bot<token>/getUpdates).
const stallTestToken = "000000000:stall-test-token"

// newStallServer serves a valid getMe and then hangs forever on getUpdates,
// simulating a half-dead keep-alive TCP connection after a network blip.
// The returned channel signals when the wedged getUpdates request has been
// accepted (the poll loop is blocked inside it).
func newStallServer(t *testing.T) (srv *httptest.Server, wedged <-chan struct{}) {
	t.Helper()

	wedgedCh := make(chan struct{})
	var once sync.Once

	// stallClosed unblocks the wedged handler during t.Cleanup, so
	// httptest.Server.Close can terminate (a handler that never returns
	// would make Server.Close hang forever — the test would mask its own
	// result the same way the real daemon masked the hang until it was
	// killed).
	stallClosed := make(chan struct{})

	mux := http.NewServeMux()
	mux.HandleFunc("/bot"+stallTestToken+"/getMe", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"result":{"id":1,"is_bot":true,"first_name":"t","username":"t"}}`))
	})
	mux.HandleFunc("/bot"+stallTestToken+"/getUpdates", func(_ http.ResponseWriter, _ *http.Request) {
		once.Do(func() { close(wedgedCh) })
		<-stallClosed // block until test cleanup — no response, no close
	})

	srv = httptest.NewServer(mux)
	// Cleanups run LIFO: stallClosed must be closed BEFORE srv.Close waits
	// on the wedged handler, so it is registered after it.
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(stallClosed) })

	return srv, wedgedCh
}

// TestCloseReturnsWhileGetUpdatesStalled reproduces the daemon-shutdown hang
// seen on 2026-09-18: after a network reset the long-poll GetUpdates request
// stayed wedged on a half-dead TCP connection, and Channel.Close() — which
// waits on the poll-loop goroutine via wg.Wait() — blocked forever, forcing
// the operator to kill ready serve. With a client-side HTTP timeout the
// wedged request unwinds and Close returns promptly.
func TestCloseReturnsWhileGetUpdatesStalled(t *testing.T) {
	// Override the client seam with a short timeout so the wedged request
	// unwinds quickly in the test (production uses telegramHTTPTimeout).
	origClient := newHTTPClient
	newHTTPClient = func() *http.Client { return &http.Client{Timeout: 500 * time.Millisecond} }
	t.Cleanup(func() { newHTTPClient = origClient })

	srv, wedged := newStallServer(t)

	bot, err := tgbotapi.NewBotAPIWithClient(stallTestToken, srv.URL+"/bot%s/%s", newHTTPClient())
	if err != nil {
		t.Fatalf("NewBotAPIWithClient: %v", err)
	}

	ch := newChannel(&botWrapper{api: bot, token: stallTestToken, hc: newHTTPClient()}, nil, zerolog.Nop())

	// Wait until the poll loop is wedged inside the stalled GetUpdates
	// request, so Close must actually wait on it (otherwise a race lets
	// Close win before the loop enters the request).
	<-wedged

	done := make(chan error, 1)
	go func() {
		done <- ch.Close()
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Close returned error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Close did not return within 3s while GetUpdates is stalled — shutdown hang reproduced")
	}
}

// TestProductionHTTPClientHasTimeout guards the fix at the seam level: the
// client built by production's newHTTPClient must carry a timeout larger
// than the 10 s long-poll window, or Channel.Close() can block forever on a
// wedged request (the 2026-09-18 daemon hang).
func TestProductionHTTPClientHasTimeout(t *testing.T) {
	hc := newHTTPClient()

	if hc.Timeout <= 0 {
		t.Fatal("production HTTP client has no timeout — shutdown hang returns")
	}

	// Must stay above updateCfg.Timeout (10 s long-poll window) so a normal
	// poll is never cut short by the client.
	if hc.Timeout <= telegramPollWindow {
		t.Fatalf("HTTP client timeout %v must exceed the long-poll window %v", hc.Timeout, telegramPollWindow)
	}
}
