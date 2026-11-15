// Package arrmada reads an Arrmada server's history and library to find the
// folders and files that changed since the last poll. Paths are returned as
// Arrmada sees them; Silo applies the source's path rewrites.
package arrmada

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// maxResponseBody caps one response at 16 MiB. A series' detail lists every
// episode with its overview, so a long-running show is the largest read.
const maxResponseBody = 16 << 20

// maxErrorBody is how much of an error response goes into the error text.
const maxErrorBody = 512

// requestTimeout bounds one request. Silo gives a whole poll five minutes.
const requestTimeout = 30 * time.Second

// errNotFound is a 404: the movie or series was removed from Arrmada.
var errNotFound = errors.New("arrmada: not found")

// client reads Arrmada's /api/v1 with a personal API key. Arrmada refuses a
// key in the query string, so it goes in the X-Api-Key header.
type client struct {
	baseURL    string
	apiKey     string
	httpClient *http.Client
}

func newClient(baseURL, apiKey string, httpClient *http.Client) (*client, error) {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	apiKey = strings.TrimSpace(apiKey)
	if baseURL == "" {
		return nil, errors.New("the connection has no Arrmada address")
	}
	if apiKey == "" {
		return nil, errors.New("the connection has no Arrmada API key")
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: requestTimeout}
	}
	return &client{baseURL: baseURL, apiKey: apiKey, httpClient: httpClient}, nil
}

// getJSON GETs path (which starts with /api/v1) and decodes the body into dest.
func (c *client) getJSON(ctx context.Context, path string, dest any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return fmt.Errorf("arrmada: build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Api-Key", c.apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("arrmada: %w", err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusNotFound:
		return errNotFound
	case resp.StatusCode == http.StatusUnauthorized:
		return errors.New("arrmada: the API key was refused (HTTP 401)")
	case resp.StatusCode == http.StatusForbidden:
		return errors.New("arrmada: the API key's account is not a manager or admin (HTTP 403)")
	case resp.StatusCode >= 400:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
		return fmt.Errorf("arrmada: HTTP %d from %s: %s", resp.StatusCode, pathOnly(path), strings.TrimSpace(string(body)))
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBody)).Decode(dest); err != nil {
		return fmt.Errorf("arrmada: read %s: %w", pathOnly(path), err)
	}
	return nil
}

// pathOnly drops the query from path for error text.
func pathOnly(path string) string {
	p, _, _ := strings.Cut(path, "?")
	return p
}
