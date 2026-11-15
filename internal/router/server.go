// Package router is the request_router.v1 capability: it sends Silo's movie
// and series requests to Arrmada's own requests, and reports their progress
// back. Every call carries its connections; the package stores nothing.
package router

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/zorcerer/silo-plugin-arrmada/internal/arrmada"
)

// Server implements request_router.v1.
type Server struct {
	pluginv1.UnimplementedRequestRouterServer

	// open builds the Arrmada client; tests swap it for one with their own
	// HTTP client.
	open func(baseURL string, cred arrmada.Credential) (*arrmada.Requests, error)
}

// New returns a ready request router.
func New() *Server {
	return &Server{open: func(baseURL string, cred arrmada.Credential) (*arrmada.Requests, error) {
		return arrmada.NewRequests(baseURL, cred, nil)
	}}
}

// config is the connection form's values.
type config struct {
	Supports4K       bool
	QualityProfile   string
	QualityProfile4K string
	Approve          bool
}

func readConfig(c *pluginv1.RouterConnection) config {
	cfg := config{Approve: true}
	if c.GetConfig() == nil {
		return cfg
	}
	m := c.GetConfig().AsMap()
	if v, ok := m["supports_4k"].(bool); ok {
		cfg.Supports4K = v
	}
	if v, ok := m["approve"].(bool); ok {
		cfg.Approve = v
	}
	if v, ok := m["quality_profile"].(string); ok {
		cfg.QualityProfile = strings.TrimSpace(v)
	}
	if v, ok := m["quality_profile_4k"].(string); ok {
		cfg.QualityProfile4K = strings.TrimSpace(v)
	}
	return cfg
}

func (s *Server) client(c *pluginv1.RouterConnection) (*arrmada.Requests, error) {
	cred, err := arrmada.ParseCredential(c.GetApiKey())
	if err != nil {
		return nil, err
	}
	return s.open(c.GetBaseUrl(), cred)
}

// Fulfill creates one Arrmada request for the title. Arrmada keeps one
// request per title, so of the qualities Silo asks for, the first HD one is
// sent, or the first 4K one when the connection handles 4K and no HD one was
// asked for. The others get no target.
func (s *Server) Fulfill(ctx context.Context, req *pluginv1.FulfillRequest) (*pluginv1.FulfillResponse, error) {
	d := req.GetRequest()
	var conn *pluginv1.RouterConnection
	for _, c := range req.GetConnections() {
		if c.GetBaseUrl() != "" {
			conn = c
			break
		}
	}
	if conn == nil {
		return &pluginv1.FulfillResponse{Message: "no Arrmada server is configured"}, nil
	}
	mediaType := d.GetMediaType()
	if mediaType != "movie" && mediaType != "series" {
		return &pluginv1.FulfillResponse{Message: fmt.Sprintf("Arrmada does not take %q requests", mediaType)}, nil
	}
	tmdb, _ := strconv.Atoi(d.GetExternalIds()["tmdb"])
	if tmdb <= 0 {
		return &pluginv1.FulfillResponse{Message: "Arrmada needs the title's TMDB id, which this request does not have"}, nil
	}
	cfg := readConfig(conn)
	quality, profile := pickQuality(req.GetQualities(), cfg)
	if quality == nil {
		return &pluginv1.FulfillResponse{Message: "this Arrmada connection does not handle 4K"}, nil
	}

	api, err := s.client(conn)
	if err != nil {
		return nil, err
	}
	in := arrmada.NewRequest{
		MediaType:      mediaType,
		TMDBID:         tmdb,
		Title:          d.GetTitle(),
		Year:           int(d.GetYear()),
		QualityProfile: profile,
	}
	if who := d.GetRequesterUsername(); who != "" {
		in.Note = "Requested in Silo by " + who
	}
	target := &pluginv1.FulfillmentTarget{Quality: quality.GetId(), ConnectionId: conn.GetId()}

	created, err := api.Create(ctx, in)
	var se *arrmada.StatusError
	switch {
	case errors.As(err, &se) && se.Code == 409 && strings.Contains(se.Message, "already in the library"):
		target.Status, target.Message = "completed", "Already in Arrmada's library"
		return &pluginv1.FulfillResponse{Targets: []*pluginv1.FulfillmentTarget{target}}, nil
	case errors.As(err, &se):
		// Arrmada answered and refused: a quota, a declined title, an adult
		// title. Asking again will not change that.
		target.Status, target.Message = "failed", strings.TrimPrefix(se.Message, "arrmada: ")
		return &pluginv1.FulfillResponse{Targets: []*pluginv1.FulfillmentTarget{target}}, nil
	case err != nil:
		return nil, err
	}

	if created.Status == "pending" && cfg.Approve {
		// Approval needs a manager or admin key with full access. When it is
		// refused the request waits in Arrmada for staff, which is reported.
		if err := api.Approve(ctx, created.ID, profile); err == nil {
			if again, gerr := api.Get(ctx, created.ID); gerr == nil {
				created = again
			} else {
				created.Status = "approved"
			}
		}
	}
	target.ExternalId = strconv.FormatInt(created.ID, 10)
	st := statusOf(created)
	target.Status, target.ExternalStatus, target.Message, target.Wording = st.Status, st.ExternalStatus, st.Message, st.Wording
	return &pluginv1.FulfillResponse{Targets: []*pluginv1.FulfillmentTarget{target}}, nil
}

// pickQuality returns the quality to send and its Arrmada profile, or nil.
func pickQuality(qs []*pluginv1.RequestedQuality, cfg config) (*pluginv1.RequestedQuality, string) {
	for _, q := range qs {
		if !q.GetIs4K() {
			return q, cfg.QualityProfile
		}
	}
	if cfg.Supports4K {
		for _, q := range qs {
			if q.GetIs4K() {
				return q, cfg.QualityProfile4K
			}
		}
	}
	return nil, ""
}

// CheckStatus reads each target's Arrmada request.
func (s *Server) CheckStatus(ctx context.Context, req *pluginv1.CheckStatusRequest) (*pluginv1.CheckStatusResponse, error) {
	conns := map[string]*pluginv1.RouterConnection{}
	for _, c := range req.GetConnections() {
		conns[c.GetId()] = c
	}
	resp := &pluginv1.CheckStatusResponse{}
	for _, t := range req.GetTargets() {
		st, err := s.checkOne(ctx, t, conns)
		if err != nil {
			return nil, err
		}
		st.Quality, st.ConnectionId = t.GetQuality(), t.GetConnectionId()
		resp.Statuses = append(resp.Statuses, st)
	}
	return resp, nil
}

func (s *Server) checkOne(ctx context.Context, t *pluginv1.TargetRef, conns map[string]*pluginv1.RouterConnection) (*pluginv1.TargetStatus, error) {
	if t.GetExternalId() == "" {
		// Fulfill found it already in the library.
		return &pluginv1.TargetStatus{Status: "completed"}, nil
	}
	conn := conns[t.GetConnectionId()]
	if conn == nil {
		return &pluginv1.TargetStatus{Status: "failed", Message: "its Arrmada connection was removed"}, nil
	}
	id, err := strconv.ParseInt(t.GetExternalId(), 10, 64)
	if err != nil {
		return &pluginv1.TargetStatus{Status: "failed", Message: "unreadable Arrmada request id " + strconv.Quote(t.GetExternalId())}, nil
	}
	api, err := s.client(conn)
	if err != nil {
		return nil, err
	}
	r, err := api.Get(ctx, id)
	if errors.Is(err, arrmada.ErrRequestGone) {
		return &pluginv1.TargetStatus{Status: "failed", Message: "the request was deleted in Arrmada"}, nil
	}
	if err != nil {
		return nil, err
	}
	return statusOf(r), nil
}

// statusOf maps an Arrmada request onto Silo's four statuses. The stage goes
// into external_status as Arrmada names it.
func statusOf(r arrmada.Request) *pluginv1.TargetStatus {
	out := &pluginv1.TargetStatus{ExternalStatus: r.Status}
	switch r.Status {
	case "declined":
		out.Status, out.Message = "failed", "Declined in Arrmada"
		return out
	case "pending":
		out.Status = "queued"
		out.Wording = &pluginv1.RequestStatusWording{Label: "Awaiting approval", Detail: "Waiting for approval in Arrmada."}
		return out
	}
	t := r.Tracking
	if t == nil {
		out.Status = "queued"
		return out
	}
	out.ExternalStatus = t.Stage
	phase := ""
	switch t.Stage {
	case "available":
		out.Status = "completed"
		return out
	case "declined":
		out.Status, out.Message = "failed", "Declined in Arrmada"
		return out
	case "pending", "searching":
		out.Status = "queued"
	case "queued":
		out.Status, phase = "downloading", "queued"
	case "downloading":
		out.Status, phase = "downloading", "downloading"
	case "paused":
		out.Status, phase = "downloading", "paused"
	case "failed":
		// Arrmada retries a failed download with another release.
		out.Status, phase = "downloading", "stalled"
	case "importing", "adding":
		out.Status, phase = "downloading", "importing"
	case "partial":
		out.Status = "downloading"
	default:
		out.Status = "queued"
	}
	if t.Stalled && phase != "" {
		phase = "stalled"
	}
	if note := strings.TrimSpace(t.Note); note != "" && len(note) <= 24 && !strings.ContainsAny(note, "\r\n") {
		out.Wording = &pluginv1.RequestStatusWording{Label: note}
	}
	if phase != "" && (t.Downloads > 0 || t.SizeBytes > 0) {
		p := &pluginv1.DownloadProgress{Phase: phase, Downloads: int32(t.Downloads)}
		if t.SizeBytes > 0 {
			frac := min(max(t.Progress, 0), 1)
			p.BytesTotal = t.SizeBytes
			p.BytesLeft = t.SizeBytes - int64(float64(t.SizeBytes)*frac)
		}
		if t.ETASeconds > 0 {
			p.EstimatedCompletion = timestamppb.New(time.Now().Add(time.Duration(t.ETASeconds) * time.Second))
		}
		out.Progress = p
	}
	return out
}

// TestConnection signs in and says as whom. A failure is returned as an
// InvalidArgument status: Silo shows that text on the form as it is, where a
// failed response is rewritten into one of its own generic messages.
func (s *Server) TestConnection(ctx context.Context, req *pluginv1.TestConnectionRequest) (*pluginv1.TestConnectionResponse, error) {
	api, err := s.client(req.GetConnection())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	me, err := api.Me(ctx)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, strings.TrimPrefix(err.Error(), "arrmada: "))
	}
	msg := fmt.Sprintf("Connected to Arrmada as %s (%s)", me.Username, me.Role)
	if readConfig(req.GetConnection()).Approve && me.Role != "manager" && me.Role != "admin" {
		msg += ". Requests from this account wait for staff approval in Arrmada unless it auto-approves"
	}
	return &pluginv1.TestConnectionResponse{Ok: true, Message: msg}, nil
}

// ListConfigOptions has nothing to list: the connection form has no dynamic
// selects, since Arrmada's keys cannot read its quality profiles.
func (s *Server) ListConfigOptions(context.Context, *pluginv1.ListConfigOptionsRequest) (*pluginv1.ListConfigOptionsResponse, error) {
	return &pluginv1.ListConfigOptionsResponse{}, nil
}

// Validate checks the key field's shape.
func (s *Server) Validate(_ context.Context, req *pluginv1.ValidateRequest) (*pluginv1.ValidateResponse, error) {
	resp := &pluginv1.ValidateResponse{}
	if key := req.GetConnection().GetApiKey(); key != "" {
		if _, err := arrmada.ParseCredential(key); err != nil {
			resp.FormError = err.Error()
		}
	}
	return resp, nil
}
