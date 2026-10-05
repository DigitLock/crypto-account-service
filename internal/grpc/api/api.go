// Package api is the gRPC API of server: authentication and the services of cas.v1 (SRS — Core §2.1.1).
package api

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/reflection"
	"google.golang.org/grpc/status"

	"github.com/DigitLock/crypto-account-service/internal/auth"
	casv1 "github.com/DigitLock/crypto-account-service/internal/grpc/pb/cas/v1"
	"github.com/DigitLock/crypto-account-service/internal/registry"
	"github.com/DigitLock/crypto-account-service/internal/repository"
)

// Messages of the authentication errors. The UNAUTHENTICATED message is the same for every reason.
const (
	msgUnauthenticated = "missing or invalid service token"
	msgInternal        = "internal error"
	tenantActive       = "ACTIVE"
	bearerPrefix       = "Bearer "
)

// CredentialStore loads the credential of a key_id; *repository.Queries satisfies it.
type CredentialStore interface {
	GetServiceTokenByKeyID(ctx context.Context, keyID string) (repository.GetServiceTokenByKeyIDRow, error)
}

// Deps are the dependencies of the server, given in one place.
type Deps struct {
	// Credentials loads the credential of a token: the queries on the pool of cas_server.
	Credentials CredentialStore
	// Connections manages the connections of a tenant.
	Connections *registry.Connections
	Logger      *slog.Logger
	// Metrics receives grpc_request_seconds (SRS — Core §2.5.1); nil keeps it unregistered.
	Metrics prometheus.Registerer
}

// NewServer returns the gRPC server of cas.v1: authentication on every unary call, the services, and
// server reflection, which needs no token. cmd/server and the tests use it.
func NewServer(deps Deps, opts ...grpc.ServerOption) *grpc.Server {
	latency := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name: "grpc_request_seconds", Help: "API latency by method: the service and the method.",
		Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.3, 0.5, 1, 2.5, 5},
	}, []string{"method"})
	if deps.Metrics != nil {
		deps.Metrics.MustRegister(latency)
	}
	// The latency interceptor runs first, so requests refused by authentication are measured too.
	opts = append([]grpc.ServerOption{grpc.ChainUnaryInterceptor(
		UnaryLatency(latency), UnaryAuth(deps.Credentials, deps.Logger),
	)}, opts...)
	srv := grpc.NewServer(opts...)
	casv1.RegisterConnectionServiceServer(srv, &connectionService{conns: deps.Connections, logger: deps.Logger})
	casv1.RegisterAccountDataServiceServer(srv, casv1.UnimplementedAccountDataServiceServer{})
	reflection.Register(srv)
	return srv
}

// UnaryLatency observes the duration of every unary call in grpc_request_seconds{method}.
func UnaryLatency(latency *prometheus.HistogramVec) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		start := time.Now()
		resp, err := handler(ctx, req)
		latency.WithLabelValues(MethodLabel(info.FullMethod)).Observe(time.Since(start).Seconds())
		return resp, err
	}
}

// MethodLabel turns "/cas.v1.ConnectionService/ListSources" into "ConnectionService/ListSources".
func MethodLabel(fullMethod string) string {
	service, method, ok := strings.Cut(strings.TrimPrefix(fullMethod, "/"), "/")
	if !ok {
		return fullMethod
	}
	if i := strings.LastIndex(service, "."); i >= 0 {
		service = service[i+1:]
	}
	return service + "/" + method
}

type ctxKey int

const (
	tenantKey ctxKey = iota
	credentialKey
)

// TenantID returns the tenant of the authenticated caller.
func TenantID(ctx context.Context) (uuid.UUID, bool) {
	id, ok := ctx.Value(tenantKey).(uuid.UUID)
	return id, ok
}

// CredentialID returns the credential of the authenticated caller.
func CredentialID(ctx context.Context) (uuid.UUID, bool) {
	id, ok := ctx.Value(credentialKey).(uuid.UUID)
	return id, ok
}

// UnaryAuth authenticates every unary call with the metadata `authorization: Bearer <token>`:
// one database query per call, no cache. Nothing of the request metadata is logged.
func UnaryAuth(store CredentialStore, logger *slog.Logger) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		cred, err := authenticate(ctx, store)
		if err != nil {
			if _, isStatus := status.FromError(err); isStatus {
				return nil, err
			}
			// A database failure: logged here, never returned to the caller.
			logger.ErrorContext(ctx, "authentication lookup failed", "error", err)
			return nil, status.Error(codes.Internal, msgInternal)
		}
		ctx = context.WithValue(ctx, tenantKey, cred.TenantID)
		ctx = context.WithValue(ctx, credentialKey, cred.ID)
		return handler(ctx, req)
	}
}

// authenticate returns the credential of the call, a status error, or a database error.
func authenticate(ctx context.Context, store CredentialStore) (repository.GetServiceTokenByKeyIDRow, error) {
	var none repository.GetServiceTokenByKeyIDRow
	unauthenticated := status.Error(codes.Unauthenticated, msgUnauthenticated)

	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return none, unauthenticated
	}
	values := md.Get("authorization")
	if len(values) != 1 || !strings.HasPrefix(values[0], bearerPrefix) {
		return none, unauthenticated
	}
	keyID, secret, err := auth.Parse(strings.TrimPrefix(values[0], bearerPrefix))
	if err != nil {
		return none, unauthenticated
	}

	cred, err := store.GetServiceTokenByKeyID(ctx, keyID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return none, unauthenticated
	case err != nil && ctx.Err() != nil:
		return none, status.FromContextError(ctx.Err()).Err()
	case err != nil:
		return none, err
	}
	if !auth.Verify(secret, cred.SecretHash) || cred.RevokedAt != nil || cred.TenantStatus != tenantActive {
		return none, unauthenticated
	}
	return cred, nil
}
