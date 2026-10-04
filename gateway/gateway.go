// Package gateway implements the filtered CalDAV-to-ICS gateway: fetching
// a backend calendar over HTTP Basic auth, filtering events by regex, and
// serving health/readiness checks.
package gateway

import (
	"bytes"
	"compress/gzip"
	"io"
	"log"
	"net/http"
	"regexp"
	"sync"
)

// Config holds the gateway's runtime settings. SecretPath is the exact
// path (including leading slash) that serves the filtered calendar.
type Config struct {
	SecretPath  string
	BackendURL  string
	BackendUser string
	BackendPass string
	Allow       *regexp.Regexp
	Deny        *regexp.Regexp
}

// Gateway serves a filtered calendar fetched from a CalDAV backend, plus
// health/readiness checks. It caches the backend's last response in memory
// and uses conditional (If-Modified-Since) requests to avoid re-fetching
// and re-filtering unchanged calendars; the cache is invisible to clients.
// The zero value is not usable; construct with New.
type Gateway struct {
	cfg    Config
	client *http.Client
	mux    *http.ServeMux
	cache  calendarCache
}

// New builds a Gateway. client is used for all backend requests.
func New(cfg Config, client *http.Client) *Gateway {
	g := &Gateway{cfg: cfg, client: client, mux: http.NewServeMux()}
	g.mux.HandleFunc("/health", g.handleHealth)
	g.mux.HandleFunc("/ready", g.handleReady)
	g.mux.HandleFunc(g.cfg.SecretPath, g.handleCalendar)
	return g
}

// ServeHTTP implements http.Handler, routing to /health, /ready, and the
// secret calendar path.
func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.mux.ServeHTTP(w, r)
}

// calendarCache holds the most recently fetched, already-filtered calendar
// body, gzip-compressed, along with the backend's Last-Modified header, so
// later requests can ask the backend "has this changed since
// <lastModified>?" instead of re-fetching and re-filtering every time.
// Safe for concurrent use; reads and writes are independent, so concurrent
// requests are never serialized against each other.
type calendarCache struct {
	mu           sync.RWMutex
	lastModified string
	compressed   []byte
}

// get returns the cached Last-Modified value and an io.ReadCloser that
// decompresses the cached body on read. ok is false if nothing is cached
// yet, in which case body is nil.
func (c *calendarCache) get() (lastModified string, body io.ReadCloser, ok bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.compressed == nil {
		return "", nil, false
	}
	gz, err := gzip.NewReader(bytes.NewReader(c.compressed))
	if err != nil {
		return "", nil, false
	}
	return c.lastModified, gz, true
}

// set gzip-compresses body and stores it alongside lastModified.
func (c *calendarCache) set(lastModified string, body []byte) error {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if _, err := gz.Write(body); err != nil {
		return err
	}
	if err := gz.Close(); err != nil {
		return err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	c.lastModified = lastModified
	c.compressed = buf.Bytes()
	return nil
}

func (g *Gateway) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
}

func (g *Gateway) handleReady(w http.ResponseWriter, r *http.Request) {
	req, err := http.NewRequest(http.MethodGet, g.cfg.BackendURL, nil)
	if err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	req.SetBasicAuth(g.cfg.BackendUser, g.cfg.BackendPass)

	resp, err := g.client.Do(req)
	if err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		w.WriteHeader(http.StatusOK)
	} else {
		w.WriteHeader(http.StatusServiceUnavailable)
	}
}

func (g *Gateway) handleCalendar(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != g.cfg.SecretPath {
		http.NotFound(w, r)
		return
	}

	cachedLastModified, cachedBody, haveCached := g.cache.get()

	req, err := http.NewRequest(http.MethodGet, g.cfg.BackendURL, nil)
	if err != nil {
		closeIfPresent(cachedBody)
		http.Error(w, "backend request failed", http.StatusBadGateway)
		return
	}
	req.SetBasicAuth(g.cfg.BackendUser, g.cfg.BackendPass)
	if haveCached && cachedLastModified != "" {
		req.Header.Set("If-Modified-Since", cachedLastModified)
	}

	resp, err := g.client.Do(req)
	if err != nil {
		closeIfPresent(cachedBody)
		http.Error(w, "backend unreachable", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusNotModified && haveCached:
		io.Copy(io.Discard, resp.Body)
		defer cachedBody.Close()
		w.Header().Set("Content-Type", "text/calendar; charset=utf-8")
		io.Copy(w, cachedBody)
	case resp.StatusCode == http.StatusOK:
		closeIfPresent(cachedBody)
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			http.Error(w, "backend error", http.StatusBadGateway)
			return
		}
		filtered := filterICS(body, g.cfg.Allow, g.cfg.Deny)
		if err := g.cache.set(resp.Header.Get("Last-Modified"), filtered); err != nil {
			log.Printf("gateway: failed to cache calendar: %v", err)
		}
		w.Header().Set("Content-Type", "text/calendar; charset=utf-8")
		w.Write(filtered)
	default:
		closeIfPresent(cachedBody)
		io.Copy(io.Discard, resp.Body)
		http.Error(w, "backend error", http.StatusBadGateway)
	}
}

func closeIfPresent(c io.Closer) {
	if c != nil {
		c.Close()
	}
}

// filterICS keeps VEVENT blocks whose SUMMARY passes the allow/deny regexps,
// leaving everything outside VEVENT blocks untouched.
func filterICS(ics []byte, allow, deny *regexp.Regexp) []byte {
	lines := unfoldLines(ics)

	var out bytes.Buffer
	var event [][]byte
	inEvent := false

	flushEvent := func() {
		if len(event) == 0 {
			return
		}
		summary := eventSummary(event)
		keep := allow == nil || allow.MatchString(summary)
		if keep && deny != nil && deny.MatchString(summary) {
			keep = false
		}
		if keep {
			for _, l := range event {
				out.Write(l)
				out.WriteString("\r\n")
			}
		}
		event = nil
	}

	for _, line := range lines {
		trimmed := bytes.TrimRight(line, "\r")
		switch {
		case bytes.Equal(trimmed, []byte("BEGIN:VEVENT")):
			inEvent = true
			event = append(event, trimmed)
		case bytes.Equal(trimmed, []byte("END:VEVENT")):
			event = append(event, trimmed)
			inEvent = false
			flushEvent()
		case inEvent:
			event = append(event, trimmed)
		default:
			out.Write(trimmed)
			out.WriteString("\r\n")
		}
	}

	return out.Bytes()
}

func eventSummary(event [][]byte) string {
	for _, l := range event {
		if bytes.HasPrefix(l, []byte("SUMMARY:")) || bytes.HasPrefix(l, []byte("SUMMARY;")) {
			idx := bytes.IndexByte(l, ':')
			if idx >= 0 {
				return string(l[idx+1:])
			}
		}
	}
	return ""
}

// unfoldLines splits raw ICS text into logical lines, undoing RFC 5545
// line folding (continuation lines start with a space or tab).
func unfoldLines(ics []byte) [][]byte {
	raw := bytes.Split(bytes.ReplaceAll(ics, []byte("\r\n"), []byte("\n")), []byte("\n"))

	var lines [][]byte
	for _, l := range raw {
		if len(l) > 0 && (l[0] == ' ' || l[0] == '\t') && len(lines) > 0 {
			lines[len(lines)-1] = append(lines[len(lines)-1], l[1:]...)
			continue
		}
		lines = append(lines, l)
	}
	return lines
}
