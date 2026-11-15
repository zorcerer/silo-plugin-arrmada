package arrmada

import (
	"context"
	"errors"
	"net/http"
	"strconv"
)

// Request is the part of an Arrmada request the router reads.
type Request struct {
	ID       int64     `json:"id"`
	Status   string    `json:"status"` // pending | approved | declined
	Tracking *Tracking `json:"tracking"`
}

// Tracking is where an approved request has got to.
type Tracking struct {
	// Stage is pending, declined, searching, queued, downloading, paused,
	// failed, importing, partial, adding or available.
	Stage      string  `json:"stage"`
	Progress   float64 `json:"progress"`
	ETASeconds int64   `json:"eta_seconds"`
	SizeBytes  int64   `json:"size_bytes"`
	Downloads  int     `json:"downloads"`
	Note       string  `json:"note"`
	Stalled    bool    `json:"stalled"`
}

// NewRequest is the body of POST /api/v1/requests.
type NewRequest struct {
	MediaType      string `json:"media_type"`
	TMDBID         int    `json:"tmdb_id"`
	Title          string `json:"title"`
	Year           int    `json:"year,omitempty"`
	Note           string `json:"note,omitempty"`
	QualityProfile string `json:"quality_profile,omitempty"`
	Seasons        []int  `json:"seasons,omitempty"`
}

// Requests calls Arrmada's requests routes, which a personal API key reaches.
type Requests struct{ c *client }

// NewRequests returns a client for one connection.
func NewRequests(baseURL string, cred Credential, httpClient *http.Client) (*Requests, error) {
	c, err := newClient(baseURL, cred, httpClient)
	if err != nil {
		return nil, err
	}
	return &Requests{c: c}, nil
}

// Me is the account the credential acts as.
type Me struct {
	Username string `json:"username"`
	Role     string `json:"role"`
}

// Me reads GET /api/v1/auth/me.
func (r *Requests) Me(ctx context.Context) (Me, error) {
	var out struct {
		User Me `json:"user"`
	}
	err := r.c.getJSON(ctx, "/api/v1/auth/me", &out)
	return out.User, err
}

// Create posts a request. When the title was already requested, Arrmada
// follows the existing request and answers with it, so a repeat converges.
func (r *Requests) Create(ctx context.Context, in NewRequest) (Request, error) {
	var out struct {
		Request Request `json:"request"`
	}
	err := r.c.postJSON(ctx, "/api/v1/requests", in, &out)
	return out.Request, err
}

// Approve approves a pending request with profile ("" is the default
// profile). It needs a manager or admin key with full access.
func (r *Requests) Approve(ctx context.Context, id int64, profile string) error {
	body := map[string]string{}
	if profile != "" {
		body["quality_profile"] = profile
	}
	return r.c.postJSON(ctx, "/api/v1/requests/"+strconv.FormatInt(id, 10)+"/approve", body, nil)
}

// Get reads one request. ErrRequestGone means it was deleted in Arrmada.
func (r *Requests) Get(ctx context.Context, id int64) (Request, error) {
	var out struct {
		Request Request `json:"request"`
	}
	err := r.c.getJSON(ctx, "/api/v1/requests/"+strconv.FormatInt(id, 10), &out)
	if errors.Is(err, errNotFound) {
		return Request{}, ErrRequestGone
	}
	return out.Request, err
}

// ErrRequestGone is a request Arrmada no longer has.
var ErrRequestGone = errors.New("the request was deleted in Arrmada")
