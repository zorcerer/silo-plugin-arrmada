// Package arrmada talks to an Arrmada server's /api/v1: its history and
// library for Autoscan, and its requests for the request router. Paths are
// returned as Arrmada sees them; Silo applies the source's path rewrites.
package arrmada

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// maxResponseBody caps one response at 16 MiB. A series' detail lists every
// episode with its overview, so a long-running show is the largest read.
const maxResponseBody = 16 << 20

// maxErrorBody is how much of an error response goes into the error text.
const maxErrorBody = 512

// requestTimeout bounds one request. Silo gives a whole poll five minutes.
const requestTimeout = 30 * time.Second

// sessionCookie is the cookie Arrmada signs a browser in with.
const sessionCookie = "arrmada_session"

// userAgent names the plugin in Arrmada's device list for the session.
const userAgent = "silo-plugin-arrmada"

var errNotFound = errors.New("arrmada: not found")

// Credential is what the operator put in the connection's API key field:
// a personal API key (arrk_...) or an account as username:password.
type Credential struct {
	APIKey   string
	Username string
	Password string
}

// ParseCredential reads the connection's API key field. A value that starts
// with arrk_ is a key; anything else with a colon is username:password, split
// at the first colon so a password may contain one.
func ParseCredential(s string) (Credential, error) {
	s = strings.TrimSpace(s)
	switch {
	case s == "":
		return Credential{}, errors.New("the connection has no Arrmada API key or account")
	case strings.HasPrefix(s, "arrk_"):
		if !looksLikeKey(s) {
			return Credential{}, fmt.Errorf("the API key is not shaped like an Arrmada key: arrk_ followed by 52 characters from a-z and 2-7, and this one has %d after arrk_. Paste the whole key", len(s)-len("arrk_"))
		}
		return Credential{APIKey: s}, nil
	}
	user, pass, ok := strings.Cut(s, ":")
	if !ok || strings.TrimSpace(user) == "" || pass == "" {
		return Credential{}, errors.New("the connection's key is neither an Arrmada API key (arrk_...) nor username:password")
	}
	return Credential{Username: strings.TrimSpace(user), Password: pass}, nil
}

// looksLikeKey is Arrmada's own test (auth.LooksLikeAPIKey). Arrmada ignores
// a value that fails it and answers as if no key were sent.
func looksLikeKey(s string) bool {
	body, ok := strings.CutPrefix(s, "arrk_")
	if !ok || len(body) != 52 {
		return false
	}
	for i := 0; i < len(body); i++ {
		if c := body[i]; !('a' <= c && c <= 'z' || '2' <= c && c <= '7') {
			return false
		}
	}
	return true
}

// IsAccount reports whether the credential signs in with a password.
func (c Credential) IsAccount() bool { return c.Username != "" }

// client calls one Arrmada. With a key it sends X-Api-Key (Arrmada refuses a
// key in the address). With an account it signs in once and reuses the
// session cookie across polls, signing in again when Arrmada answers 401.
type client struct {
	baseURL    string
	cred       Credential
	httpClient *http.Client
}

func newClient(baseURL string, cred Credential, httpClient *http.Client) (*client, error) {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		return nil, errors.New("the connection has no Arrmada address")
	}
	if cred.APIKey == "" && !cred.IsAccount() {
		return nil, errors.New("the connection has no Arrmada API key or account")
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: requestTimeout}
	}
	return &client{baseURL: baseURL, cred: cred, httpClient: httpClient}, nil
}

// sessions holds one session cookie per Arrmada address and account, for the
// life of the plugin process. Silo restarts the process at will; the next
// call then signs in again. Each sign-in is a session in the account's device
// list, so reusing one keeps that list to one entry per plugin process.
var sessions = struct {
	sync.Mutex
	byKey map[string]string
}{byKey: map[string]string{}}

// sessionKey includes a hash of the password, so a changed password in the
// connection is a new session rather than a stale one.
func (c *client) sessionKey() string {
	sum := sha256.Sum256([]byte(c.cred.Password))
	return c.baseURL + "\x00" + c.cred.Username + "\x00" + string(sum[:8])
}

func (c *client) session() string {
	sessions.Lock()
	defer sessions.Unlock()
	return sessions.byKey[c.sessionKey()]
}

func (c *client) setSession(token string) {
	sessions.Lock()
	defer sessions.Unlock()
	if token == "" {
		delete(sessions.byKey, c.sessionKey())
	} else {
		sessions.byKey[c.sessionKey()] = token
	}
}

// signIn posts the account's password to /api/v1/auth/login and keeps the
// session cookie. A script sends neither Origin nor Sec-Fetch-Site, which
// Arrmada's cross-site check lets through.
func (c *client) signIn(ctx context.Context) error {
	body, _ := json.Marshal(map[string]string{"username": c.cred.Username, "password": c.cred.Password})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/v1/auth/login", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("arrmada: build sign-in: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("arrmada: sign in: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxErrorBody))
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return fmt.Errorf("arrmada: the password for %q was refused (status 401)", c.cred.Username)
	case resp.StatusCode == http.StatusTooManyRequests:
		return errors.New("arrmada: too many sign-in attempts from this address (status 429); try again later")
	case resp.StatusCode >= 400:
		return fmt.Errorf("arrmada: sign-in failed (status %d)", resp.StatusCode)
	}
	for _, ck := range resp.Cookies() {
		if ck.Name == sessionCookie && ck.Value != "" {
			c.setSession(ck.Value)
			return nil
		}
	}
	// Two-factor sign-in answers 200 with a ticket for the code and no cookie.
	return fmt.Errorf("arrmada: %q has two-factor sign-in on, which the plugin cannot complete; use an account without it", c.cred.Username)
}

// getJSON GETs path (which starts with /api/v1) and decodes the body into dest.
func (c *client) getJSON(ctx context.Context, path string, dest any) error {
	return c.do(ctx, http.MethodGet, path, nil, dest)
}

// postJSON POSTs in as JSON and decodes the answer into dest (nil to drop it).
func (c *client) postJSON(ctx context.Context, path string, in, dest any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return fmt.Errorf("arrmada: encode %s: %w", pathOnly(path), err)
	}
	return c.do(ctx, http.MethodPost, path, body, dest)
}

func (c *client) do(ctx context.Context, method, path string, body []byte, dest any) error {
	if c.cred.IsAccount() && c.session() == "" {
		if err := c.signIn(ctx); err != nil {
			return err
		}
	}
	resp, err := c.send(ctx, method, path, body)
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusUnauthorized && c.cred.IsAccount() {
		// The session expired or was signed out: sign in once more.
		resp.Body.Close()
		c.setSession("")
		if err := c.signIn(ctx); err != nil {
			return err
		}
		if resp, err = c.send(ctx, method, path, body); err != nil {
			return err
		}
	}
	defer resp.Body.Close()
	return c.read(resp, path, dest)
}

func (c *client) send(ctx context.Context, method, path string, body []byte) (*http.Response, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, rd)
	if err != nil {
		return nil, fmt.Errorf("arrmada: build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.cred.IsAccount() {
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: c.session()})
	} else {
		req.Header.Set("X-Api-Key", c.cred.APIKey)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("arrmada: %w", err)
	}
	// Arrmada slides a session's expiry and sends the cookie again.
	if c.cred.IsAccount() {
		for _, ck := range resp.Cookies() {
			if ck.Name == sessionCookie && ck.Value != "" {
				c.setSession(ck.Value)
			}
		}
	}
	return resp, nil
}

// StatusError is an HTTP error from Arrmada, with its message.
type StatusError struct {
	Code    int
	Message string
}

func (e *StatusError) Error() string { return e.Message }

// Is makes a 404 match errNotFound.
func (e *StatusError) Is(target error) bool {
	return target == errNotFound && e.Code == http.StatusNotFound
}

func (c *client) read(resp *http.Response, path string, dest any) error {
	if resp.StatusCode >= 400 {
		return c.statusError(resp, path)
	}
	if dest == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseBody))
		return nil
	}
	if strings.HasPrefix(strings.ToLower(resp.Header.Get("Content-Type")), "text/html") {
		return fmt.Errorf("arrmada: %s answered %s with a web page, not Arrmada's API: the address is wrong, or a sign-in page in front of Arrmada caught the request. Use an address that reaches Arrmada itself, such as http://arrmada:7878", c.baseURL, pathOnly(path))
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBody)).Decode(dest); err != nil {
		return fmt.Errorf("arrmada: read %s: %w", pathOnly(path), err)
	}
	return nil
}

// statusError explains an error answer. Arrmada answers every error with
// {"status":"error","message":...}; a 401 or 403 without that body came from
// something in front of Arrmada, such as a reverse proxy's sign-in.
//
// The text says "status 401", not "HTTP 401": Silo replaces any request
// router message containing "HTTP 401" or "HTTP 403" with "The server
// rejected this API key", which hides which of these it was.
func (c *client) statusError(resp *http.Response, path string) error {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	var body struct {
		Status  string `json:"status"`
		Message string `json:"message"`
	}
	fromArrmada := json.Unmarshal(raw, &body) == nil && body.Status == "error" && body.Message != ""
	code := resp.StatusCode
	where := pathOnly(path)

	if (code == http.StatusUnauthorized || code == http.StatusForbidden) && !fromArrmada {
		return &StatusError{code, fmt.Sprintf("arrmada: %s answered status %d to %s, but not the way Arrmada answers: something in front of Arrmada, such as a reverse proxy's sign-in, refused the request. Use an address that reaches Arrmada itself, such as http://arrmada:7878", c.baseURL, code, where)}
	}
	switch {
	case code == http.StatusUnauthorized && c.cred.IsAccount():
		return &StatusError{code, fmt.Sprintf("arrmada: the session for %q was refused (status 401): %s", c.cred.Username, body.Message)}
	case code == http.StatusUnauthorized && body.Message == "authentication required":
		// Arrmada ignores a value that is not shaped like one of its keys.
		return &StatusError{code, "arrmada: Arrmada did not recognise the API key (status 401). Paste the whole key: arrk_ followed by 52 characters"}
	case code == http.StatusUnauthorized:
		return &StatusError{code, "arrmada: Arrmada refused the API key (status 401): " + body.Message}
	case code == http.StatusForbidden && !c.cred.IsAccount():
		return &StatusError{code, fmt.Sprintf("arrmada: Arrmada refused %s for this API key (status 403): %s. Requests need a key with Full access; approving needs a manager or admin account", where, body.Message)}
	case code == http.StatusForbidden:
		return &StatusError{code, fmt.Sprintf("arrmada: Arrmada refused %s for %q (status 403): %s", where, c.cred.Username, body.Message)}
	case code == http.StatusTooManyRequests && !c.cred.IsAccount() && strings.HasPrefix(body.Message, "too many attempts"):
		// Arrmada's throttle on wrong keys; a request quota is also a 429,
		// with its own message.
		return &StatusError{code, "arrmada: too many wrong API keys from this address (status 429); wait 15 minutes and try the right key"}
	}
	text := strings.TrimSpace(string(raw))
	if fromArrmada {
		text = body.Message
	}
	return &StatusError{code, fmt.Sprintf("arrmada: status %d from %s: %s", code, where, text)}
}

// pathOnly drops the query from path for error text.
func pathOnly(path string) string {
	p, _, _ := strings.Cut(path, "?")
	return p
}
