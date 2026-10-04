// Package gateway implements the filtered CalDAV-to-ICS gateway: fetching
// a backend calendar over HTTP Basic auth, filtering events by regex, and
// serving health/readiness checks.
package gateway

import (
	"bytes"
	"compress/gzip"
	"io"
	"log/slog"
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
	Anonymize   string
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
	logger *slog.Logger
}

// New builds a Gateway. client is used for all backend requests.
func New(cfg Config, client *http.Client, logger *slog.Logger) *Gateway {
	g := &Gateway{cfg: cfg, client: client, mux: http.NewServeMux(), logger: logger}
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
		g.logger.Error("readiness check: failed to build backend request", "error", err)
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	req.SetBasicAuth(g.cfg.BackendUser, g.cfg.BackendPass)

	resp, err := g.client.Do(req)
	if err != nil {
		g.logger.Error("readiness check: backend unreachable", "error", err)
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		w.WriteHeader(http.StatusOK)
	} else {
		g.logger.Error("readiness check: backend returned error status", "status", resp.StatusCode)
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
		g.logger.Error("calendar request: failed to build backend request", "error", err)
		closeIfPresent(cachedBody)
		http.Error(w, "backend request failed", http.StatusBadGateway)
		return
	}
	req.SetBasicAuth(g.cfg.BackendUser, g.cfg.BackendPass)
	if haveCached && cachedLastModified != "" {
		req.Header.Set("If-Modified-Since", cachedLastModified)
	}

	g.logger.Info("calendar request: requesting backend", "conditional", haveCached && cachedLastModified != "")
	resp, err := g.client.Do(req)
	if err != nil {
		g.logger.Error("calendar request: backend unreachable", "error", err)
		closeIfPresent(cachedBody)
		http.Error(w, "backend unreachable", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusNotModified && haveCached:
		g.logger.Info("calendar request: cache hit")
		io.Copy(io.Discard, resp.Body)
		defer cachedBody.Close()
		w.Header().Set("Content-Type", "text/calendar; charset=utf-8")
		io.Copy(w, cachedBody)
	case resp.StatusCode == http.StatusOK:
		g.logger.Info("calendar request: cache miss")
		closeIfPresent(cachedBody)
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			g.logger.Error("calendar request: failed to read backend response", "error", err)
			http.Error(w, "backend error", http.StatusBadGateway)
			return
		}
		filtered := filterICS(body, g.cfg.Allow, g.cfg.Deny, g.cfg.Anonymize, g.logger)
		if err := g.cache.set(resp.Header.Get("Last-Modified"), filtered); err != nil {
			g.logger.Error("calendar request: failed to cache calendar", "error", err)
		}
		w.Header().Set("Content-Type", "text/calendar; charset=utf-8")
		w.Write(filtered)
	default:
		closeIfPresent(cachedBody)
		io.Copy(io.Discard, resp.Body)
		g.logger.Error("calendar request: backend returned error status", "status", resp.StatusCode)
		http.Error(w, "backend error", http.StatusBadGateway)
	}
}

func closeIfPresent(c io.Closer) {
	if c != nil {
		c.Close()
	}
}

// filterICS keeps VEVENT blocks whose SUMMARY passes the allow/deny regexps,
// leaving everything outside VEVENT blocks untouched. If anonymize is
// non-empty, surviving events have their SUMMARY, DESCRIPTION, LOCATION,
// ATTENDEE, and ORGANIZER values replaced with it, leaving timing untouched.
func filterICS(ics []byte, allow, deny *regexp.Regexp, anonymize string, logger *slog.Logger) []byte {
	lines := unfoldLines(ics)

	var out bytes.Buffer
	var event [][]byte
	inEvent := false
	var discardedByAllow, discardedByDeny int

	flushEvent := func() {
		if len(event) == 0 {
			return
		}
		summary := eventSummary(event)
		keep := allow == nil || allow.MatchString(summary)
		if !keep {
			discardedByAllow++
		} else if deny != nil && deny.MatchString(summary) {
			keep = false
			discardedByDeny++
		}
		if keep {
			if anonymize != "" {
				event = anonymizeEvent(event, anonymize)
			}
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

	logger.Info("calendar filter: filtered events",
		"allow", regexpString(allow), "discarded_by_allow", discardedByAllow,
		"deny", regexpString(deny), "discarded_by_deny", discardedByDeny)
	return out.Bytes()
}

func regexpString(re *regexp.Regexp) string {
	if re == nil {
		return ""
	}
	return re.String()
}

func eventSummary(event [][]byte) string {
	for _, l := range event {
		if hasPropertyPrefix(l, []byte("SUMMARY")) {
			idx := bytes.IndexByte(l, ':')
			if idx >= 0 {
				return string(l[idx+1:])
			}
		}
	}
	return ""
}

// anonymizedProperties get replaced wholesale by the anonymize string,
// dropping any parameters (e.g. ATTENDEE;CN=...) that might also identify
// someone.
var anonymizedProperties = [][]byte{
	[]byte("SUMMARY"), []byte("DESCRIPTION"), []byte("LOCATION"),
	[]byte("ATTENDEE"), []byte("ORGANIZER"),
}

// anonymizeEvent replaces every non-empty anonymized property in event with
// "NAME:replacement", leaving all other lines (including timing) untouched.
func anonymizeEvent(event [][]byte, replacement string) [][]byte {
	out := make([][]byte, len(event))
	for i, l := range event {
		out[i] = l
		for _, prop := range anonymizedProperties {
			if !hasPropertyPrefix(l, prop) {
				continue
			}
			idx := bytes.IndexByte(l, ':')
			if idx >= 0 && idx+1 < len(l) {
				out[i] = append(append(append([]byte{}, prop...), ':'), replacement...)
			}
			break
		}
	}
	return out
}

// hasPropertyPrefix reports whether line is the ICS property named name,
// i.e. starts with name followed by ':' (no parameters) or ';' (parameters).
func hasPropertyPrefix(line, name []byte) bool {
	if !bytes.HasPrefix(line, name) {
		return false
	}
	rest := line[len(name):]
	return len(rest) > 0 && (rest[0] == ':' || rest[0] == ';')
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
