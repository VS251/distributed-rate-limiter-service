// Package grpcserver implements the gRPC transport layer for the rate limiter service.
// It translates gRPC requests to service calls and maps service errors to gRPC status codes.
// All business logic lives in the service layer — this package only handles protocol translation.
package grpcserver

import (
	"context"
	"errors"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	ratelimiterv1 "github.com/varunsalian/ratelimiter/api/ratelimiter/v1"
	"github.com/varunsalian/ratelimiter/internal/algorithm"
	svc "github.com/varunsalian/ratelimiter/internal/service"
	"github.com/varunsalian/ratelimiter/internal/tenant"
)

// RateLimitService is the domain interface the gRPC handler delegates to.
// Defined here so the transport layer has no direct dependency on *service.Service.
type RateLimitService interface {
	CheckLimit(ctx context.Context, req svc.CheckRequest) (algorithm.Result, error)
}

// Server implements ratelimiterv1.RateLimiterServer.
type Server struct {
	ratelimiterv1.UnimplementedRateLimiterServer
	svc RateLimitService
}

// New creates a Server wired to the given domain service.
func New(s RateLimitService) *Server {
	return &Server{svc: s}
}

// CheckLimit handles the unary rate limit check RPC.
// It always returns OK status; inspect the response body to determine whether
// the request was allowed. Non-OK codes indicate service-level failures only.
func (s *Server) CheckLimit(ctx context.Context, req *ratelimiterv1.CheckLimitRequest) (*ratelimiterv1.CheckLimitResponse, error) {
	if req.TenantId == "" {
		return nil, status.Error(codes.InvalidArgument, "tenant_id is required")
	}
	if req.Key == "" {
		return nil, status.Error(codes.InvalidArgument, "key is required")
	}

	result, err := s.svc.CheckLimit(ctx, svc.CheckRequest{
		TenantID: req.TenantId,
		Key:      req.Key,
		Cost:     req.Cost,
	})
	if err != nil {
		return nil, toGRPCError(err)
	}

	resp := &ratelimiterv1.CheckLimitResponse{
		Allowed:      result.Allowed,
		Remaining:    result.Remaining,
		RetryAfterMs: result.RetryAfterMs,
	}
	// time.Time{}.UnixMilli() returns a year-1 timestamp, not zero.
	// Degrade gracefully: zero ResetAt means "unknown" — send 0 on the wire.
	if !result.ResetAt.IsZero() {
		resp.ResetAtUnixMs = result.ResetAt.UnixMilli()
	}
	return resp, nil
}

// toGRPCError maps domain errors to gRPC status codes.
//
//   - tenant.ErrNoPlan  → NOT_FOUND       (no plan configured for this tenant+key)
//   - svc.ErrInvalidConfig → INTERNAL     (server misconfiguration, not a client error)
//   - anything else     → UNAVAILABLE     (backing store unreachable, fail-closed)
func toGRPCError(err error) error {
	switch {
	case errors.Is(err, tenant.ErrNoPlan):
		return status.Errorf(codes.NotFound, "no rate limit plan configured: %v", err)
	case errors.Is(err, svc.ErrInvalidConfig):
		return status.Errorf(codes.Internal, "invalid plan configuration: %v", err)
	default:
		return status.Errorf(codes.Unavailable, "rate limit store unavailable: %v", err)
	}
}
