package gateway

import (
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
)

const fixtureICS = `BEGIN:VCALENDAR
VERSION:2.0
PRODID:-//Test//Test//EN
BEGIN:VEVENT
UID:1@example.com
DTSTART:20261010T100000Z
DTEND:20261010T110000Z
SUMMARY:Team Standup
END:VEVENT
BEGIN:VEVENT
UID:2@example.com
DTSTART:20261011T120000Z
DTEND:20261011T130000Z
SUMMARY:Secret Therapy Appointment
END:VEVENT
BEGIN:VEVENT
UID:3@example.com
DTSTART:20261012T140000Z
DTEND:20261012T150000Z
SUMMARY:Public Conference Talk
END:VEVENT
END:VCALENDAR
`

// newBackend spins up a fake CalDAV backend requiring HTTP Basic auth and
// serving fixtureICS. It fails the test if the credentials don't match.
func newBackend(t *testing.T, user, pass string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUser, gotPass, ok := r.BasicAuth()
		if !ok || gotUser != user || gotPass != pass {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "text/calendar")
		io.WriteString(w, fixtureICS)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// conditionalBackend is a fake CalDAV backend that honors If-Modified-Since
// against its own Last-Modified header, and records every request it
// receives so tests can assert on cache behavior.
type conditionalBackend struct {
	mu           sync.Mutex
	body         string
	lastModified string
	requests     []*http.Request
}

func newConditionalBackend(t *testing.T, user, pass string) (*httptest.Server, *conditionalBackend) {
	t.Helper()
	b := &conditionalBackend{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUser, gotPass, ok := r.BasicAuth()
		if !ok || gotUser != user || gotPass != pass {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}

		b.mu.Lock()
		defer b.mu.Unlock()
		b.requests = append(b.requests, r)

		if ims := r.Header.Get("If-Modified-Since"); ims != "" && ims == b.lastModified {
			w.WriteHeader(http.StatusNotModified)
			return
		}

		w.Header().Set("Content-Type", "text/calendar")
		if b.lastModified != "" {
			w.Header().Set("Last-Modified", b.lastModified)
		}
		io.WriteString(w, b.body)
	}))
	t.Cleanup(srv.Close)
	return srv, b
}

func (b *conditionalBackend) setContent(body, lastModified string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.body = body
	b.lastModified = lastModified
}

func (b *conditionalBackend) requestCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.requests)
}

func (b *conditionalBackend) lastRequestIMS() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.requests) == 0 {
		return ""
	}
	return b.requests[len(b.requests)-1].Header.Get("If-Modified-Since")
}

// newGateway instantiates a Gateway and starts it behind httptest.
func newGateway(t *testing.T, cfg Config) *httptest.Server {
	t.Helper()
	gw := New(cfg, http.DefaultClient)
	srv := httptest.NewServer(gw)
	t.Cleanup(srv.Close)
	return srv
}

func TestHealthAlwaysOK(t *testing.T) {
	t.Parallel()

	backend := newBackend(t, "u", "p")
	cfg := Config{SecretPath: "/secret", BackendURL: backend.URL, BackendUser: "u", BackendPass: "p"}
	gw := newGateway(t, cfg)

	resp, err := http.Get(gw.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("/health = %d, want 200", resp.StatusCode)
	}
}

func TestHealthOKEvenWithBackendDown(t *testing.T) {
	t.Parallel()

	backend := newBackend(t, "u", "p")
	backend.Close() // backend is dead before any request

	cfg := Config{SecretPath: "/secret", BackendURL: backend.URL, BackendUser: "u", BackendPass: "p"}
	gw := newGateway(t, cfg)

	resp, err := http.Get(gw.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("/health = %d, want 200", resp.StatusCode)
	}
}

func TestReadyReflectsBackendState(t *testing.T) {
	t.Parallel()

	backend := newBackend(t, "u", "p")
	cfg := Config{SecretPath: "/secret", BackendURL: backend.URL, BackendUser: "u", BackendPass: "p"}
	gw := newGateway(t, cfg)

	resp, err := http.Get(gw.URL + "/ready")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("/ready with backend up = %d, want 200", resp.StatusCode)
	}

	backend.Close()

	resp, err = http.Get(gw.URL + "/ready")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("/ready with backend down = %d, want 503", resp.StatusCode)
	}
}

func TestReadyFailsOnBadCredentials(t *testing.T) {
	t.Parallel()

	backend := newBackend(t, "u", "p")
	cfg := Config{SecretPath: "/secret", BackendURL: backend.URL, BackendUser: "u", BackendPass: "wrong"}
	gw := newGateway(t, cfg)

	resp, err := http.Get(gw.URL + "/ready")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("/ready with bad creds = %d, want 503", resp.StatusCode)
	}
}

func TestCalendarFiltering(t *testing.T) {
	t.Parallel()

	backend := newBackend(t, "u", "p")
	cfg := Config{
		SecretPath:  "/secret",
		BackendURL:  backend.URL,
		BackendUser: "u",
		BackendPass: "p",
		Allow:       regexp.MustCompile(`^(Team|Public)`),
		Deny:        regexp.MustCompile(`Secret`),
	}
	gw := newGateway(t, cfg)

	resp, err := http.Get(gw.URL + "/secret")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("secret path = %d, want 200", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	got := string(body)

	if !strings.Contains(got, "Team Standup") {
		t.Errorf("expected allowed event Team Standup in output, got:\n%s", got)
	}
	if !strings.Contains(got, "Public Conference Talk") {
		t.Errorf("expected allowed event Public Conference Talk in output, got:\n%s", got)
	}
	if strings.Contains(got, "Secret Therapy Appointment") {
		t.Errorf("expected denied event Secret Therapy Appointment to be filtered out, got:\n%s", got)
	}
	if !strings.Contains(got, "BEGIN:VCALENDAR") || !strings.Contains(got, "END:VCALENDAR") {
		t.Errorf("expected valid VCALENDAR wrapper, got:\n%s", got)
	}
}

func TestCalendarDefaultsToAllowAllDenyNone(t *testing.T) {
	t.Parallel()

	backend := newBackend(t, "u", "p")
	cfg := Config{SecretPath: "/secret", BackendURL: backend.URL, BackendUser: "u", BackendPass: "p"}
	gw := newGateway(t, cfg)

	resp, err := http.Get(gw.URL + "/secret")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	got := string(body)

	for _, want := range []string{"Team Standup", "Secret Therapy Appointment", "Public Conference Talk"} {
		if !strings.Contains(got, want) {
			t.Errorf("expected event %q with no filters set, got:\n%s", want, got)
		}
	}
}

func TestCalendarServesFromCacheOn304(t *testing.T) {
	t.Parallel()

	backend, fake := newConditionalBackend(t, "u", "p")
	fake.setContent(fixtureICS, "Wed, 21 Oct 2026 07:28:00 GMT")

	cfg := Config{SecretPath: "/secret", BackendURL: backend.URL, BackendUser: "u", BackendPass: "p"}
	gw := newGateway(t, cfg)

	first, err := http.Get(gw.URL + "/secret")
	if err != nil {
		t.Fatal(err)
	}
	firstBody, _ := io.ReadAll(first.Body)
	first.Body.Close()

	if fake.requestCount() != 1 {
		t.Fatalf("after first request, backend saw %d requests, want 1", fake.requestCount())
	}
	if fake.lastRequestIMS() != "" {
		t.Errorf("first request should not send If-Modified-Since, got %q", fake.lastRequestIMS())
	}

	second, err := http.Get(gw.URL + "/secret")
	if err != nil {
		t.Fatal(err)
	}
	secondBody, _ := io.ReadAll(second.Body)
	second.Body.Close()

	if fake.requestCount() != 2 {
		t.Fatalf("after second request, backend saw %d requests, want 2", fake.requestCount())
	}
	if fake.lastRequestIMS() != "Wed, 21 Oct 2026 07:28:00 GMT" {
		t.Errorf("second request If-Modified-Since = %q, want the cached Last-Modified", fake.lastRequestIMS())
	}

	// Backend replied 304 both times from the client's point of view it's
	// still a normal 200 with the same filtered body as before: caching is
	// transparent.
	if second.StatusCode != http.StatusOK {
		t.Errorf("second response status = %d, want 200", second.StatusCode)
	}
	if string(secondBody) != string(firstBody) {
		t.Errorf("second response body differs from first despite backend 304:\nfirst:\n%s\nsecond:\n%s", firstBody, secondBody)
	}
}

func TestCalendarRefetchesWhenBackendContentChanges(t *testing.T) {
	t.Parallel()

	backend, fake := newConditionalBackend(t, "u", "p")
	fake.setContent(fixtureICS, "Wed, 21 Oct 2026 07:28:00 GMT")

	cfg := Config{SecretPath: "/secret", BackendURL: backend.URL, BackendUser: "u", BackendPass: "p"}
	gw := newGateway(t, cfg)

	resp, err := http.Get(gw.URL + "/secret")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	updated := strings.Replace(fixtureICS, "Team Standup", "Team Standup Rescheduled", 1)
	fake.setContent(updated, "Thu, 22 Oct 2026 09:00:00 GMT")

	resp, err = http.Get(gw.URL + "/secret")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(string(body), "Team Standup Rescheduled") {
		t.Errorf("expected updated content after backend change, got:\n%s", body)
	}

	third, err := http.Get(gw.URL + "/secret")
	if err != nil {
		t.Fatal(err)
	}
	third.Body.Close()
	if fake.lastRequestIMS() != "Thu, 22 Oct 2026 09:00:00 GMT" {
		t.Errorf("expected gateway to cache the new Last-Modified, got IMS %q", fake.lastRequestIMS())
	}
}

func TestCalendarCacheConcurrentAccess(t *testing.T) {
	t.Parallel()

	backend, fake := newConditionalBackend(t, "u", "p")
	fake.setContent(fixtureICS, "Wed, 21 Oct 2026 07:28:00 GMT")

	cfg := Config{SecretPath: "/secret", BackendURL: backend.URL, BackendUser: "u", BackendPass: "p"}
	gw := newGateway(t, cfg)

	const workers = 50
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			resp, err := http.Get(gw.URL + "/secret")
			if err != nil {
				t.Error(err)
				return
			}
			defer resp.Body.Close()
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Error(err)
				return
			}
			if resp.StatusCode != http.StatusOK {
				t.Errorf("status = %d, want 200", resp.StatusCode)
			}
			if !strings.Contains(string(body), "Team Standup") {
				t.Errorf("missing expected event in concurrent response:\n%s", body)
			}
		}()
	}
	wg.Wait()
}

func TestUnknownPathsReturn404(t *testing.T) {
	t.Parallel()

	backend := newBackend(t, "u", "p")
	cfg := Config{SecretPath: "/secret", BackendURL: backend.URL, BackendUser: "u", BackendPass: "p"}
	gw := newGateway(t, cfg)

	for _, path := range []string{"/", "/random", "/secret/extra"} {
		resp, err := http.Get(gw.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s = %d, want 404", path, resp.StatusCode)
		}
	}
}
