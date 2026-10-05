package api

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/DigitLock/crypto-account-service/internal/connector"
	casv1 "github.com/DigitLock/crypto-account-service/internal/grpc/pb/cas/v1"
	"github.com/DigitLock/crypto-account-service/internal/registry"
	"github.com/DigitLock/crypto-account-service/internal/vault"
)

// Error detail of FAILED_PRECONDITION (SRS — Core §2.1.1).
const (
	errorDomain                = "cas"
	reasonSourceDisabled       = "SOURCE_DISABLED"
	reasonKeyInvalid           = "KEY_INVALID"
	reasonKeyNotReadOnly       = "KEY_NOT_READ_ONLY"
	reasonCredentialsInvalid   = "CREDENTIALS_INVALID"
	fingerprintPrefix          = "…"
	maxOwnerRef, maxLabel      = 128, 64
	minAPIKey, maxAPIKey       = 16, 256
	minAPISecret, maxAPISecret = 16, 4096
)

// connectionService implements ConnectionService. Methods of later stages answer UNIMPLEMENTED.
type connectionService struct {
	casv1.UnimplementedConnectionServiceServer
	conns  *registry.Connections
	logger *slog.Logger
}

// ListSources returns the available sources, ordered by code.
func (s *connectionService) ListSources(ctx context.Context, _ *casv1.ListSourcesRequest) (*casv1.ListSourcesResponse, error) {
	sources, err := s.conns.Sources(ctx)
	if err != nil {
		return nil, s.internal(ctx, "list sources", err)
	}
	resp := &casv1.ListSourcesResponse{Sources: make([]*casv1.Source, 0, len(sources))}
	for _, src := range sources {
		resp.Sources = append(resp.Sources, &casv1.Source{Code: src.Code, Kind: sourceKind(src.Kind)})
	}
	return resp, nil
}

// CreateConnection registers an exchange key or a wallet address (SRS — Core UC-101).
func (s *connectionService) CreateConnection(ctx context.Context, req *casv1.CreateConnectionRequest) (*casv1.CreateConnectionResponse, error) {
	if err := validateCreate(req); err != nil {
		return nil, err
	}
	tenantID, _ := TenantID(ctx)
	credentialID, _ := CredentialID(ctx)
	in := registry.CreateInput{
		TenantID: tenantID, CredentialID: credentialID,
		OwnerRef: req.GetOwnerRef(), Source: req.GetSource(), Label: req.GetLabel(),
	}
	if key := req.GetExchangeKey(); key != nil {
		in.Credentials.ExchangeKey = &connector.ExchangeKey{
			APIKey:    vault.NewSecret(key.GetApiKey()),
			APISecret: vault.NewSecret(key.GetApiSecret()),
		}
	} else {
		in.Credentials.WalletAddress = req.GetWallet().GetAddress()
	}

	conn, err := s.conns.Create(ctx, in)
	if err != nil {
		return nil, s.errorStatus(ctx, "create connection", err)
	}
	return &casv1.CreateConnectionResponse{Connection: toProto(conn)}, nil
}

// validateCreate is UC-101 step 1. Messages name the field, never the value of a key or a secret.
// Lengths count characters; nothing is trimmed.
func validateCreate(req *casv1.CreateConnectionRequest) error {
	invalid := func(msg string) error { return status.Error(codes.InvalidArgument, msg) }
	chars := utf8.RuneCountInString

	switch {
	case req.GetOwnerRef() == "":
		return invalid("owner_ref is required")
	case chars(req.GetOwnerRef()) > maxOwnerRef:
		return invalid("owner_ref must be at most 128 characters")
	case chars(req.GetLabel()) > maxLabel:
		return invalid("label must be at most 64 characters")
	case req.GetSource() == "":
		return invalid("source is required")
	case req.GetCredential() == nil:
		return invalid("a credential is required: exchange_key or wallet")
	}
	if key := req.GetExchangeKey(); key != nil {
		if n := chars(key.GetApiKey()); n < minAPIKey || n > maxAPIKey {
			return invalid("exchange_key.api_key must be 16 to 256 characters")
		}
		if n := chars(key.GetApiSecret()); n < minAPISecret || n > maxAPISecret {
			return invalid("exchange_key.api_secret must be 16 to 4096 characters")
		}
	}
	return nil
}

// errorStatus maps an error of the registry to its status.
func (s *connectionService) errorStatus(ctx context.Context, op string, err error) error {
	var invalid *registry.InvalidArgumentError
	var notReadOnly *registry.KeyNotReadOnlyError
	switch {
	case errors.As(err, &invalid):
		return status.Error(codes.InvalidArgument, invalid.Message)
	case errors.Is(err, registry.ErrSourceNotFound):
		return status.Error(codes.NotFound, "source not found")
	case errors.Is(err, registry.ErrSourceDisabled):
		return withReason(codes.FailedPrecondition, "the source is not available", reasonSourceDisabled)
	case errors.Is(err, registry.ErrKeyInvalid):
		return withReason(codes.FailedPrecondition, "the source rejects the key", reasonKeyInvalid)
	case errors.As(err, &notReadOnly):
		return withReason(codes.FailedPrecondition,
			"the key is not read-only: "+strings.Join(notReadOnly.Permissions, ", "), reasonKeyNotReadOnly)
	case errors.Is(err, registry.ErrUnavailable):
		return status.Error(codes.Unavailable, "the source cannot be reached; try again later")
	case errors.Is(err, registry.ErrAlreadyExists):
		return status.Error(codes.AlreadyExists, "this account is already connected")
	case errors.Is(err, registry.ErrConnectionNotFound):
		return status.Error(codes.NotFound, "connection not found")
	case errors.Is(err, registry.ErrCredentialsInvalid):
		return withReason(codes.FailedPrecondition, "the connection is stopped: its key was rejected or is no longer read-only",
			reasonCredentialsInvalid)
	case errors.Is(err, registry.ErrCooldown):
		return status.Error(codes.ResourceExhausted, "a manual sync was accepted recently: wait for the cooldown")
	case ctx.Err() != nil:
		return status.FromContextError(ctx.Err()).Err()
	default:
		return s.internal(ctx, op, err)
	}
}

// internal logs the cause, never the request, and returns the one INTERNAL message.
func (s *connectionService) internal(ctx context.Context, op string, err error) error {
	s.logger.ErrorContext(ctx, op+" failed", "error", err)
	return status.Error(codes.Internal, msgInternal)
}

func withReason(code codes.Code, msg, reason string) error {
	st, err := status.New(code, msg).WithDetails(&errdetails.ErrorInfo{Reason: reason, Domain: errorDomain})
	if err != nil {
		return status.Error(code, msg)
	}
	return st.Err()
}

func toProto(c registry.Connection) *casv1.Connection {
	out := &casv1.Connection{
		ConnectionId: c.ID.String(),
		Source:       c.Source,
		OwnerRef:     c.OwnerRef,
		Label:        c.Label,
		Status:       connectionStatus(c.Status),
		Permissions:  c.Permissions,
		CreatedAt:    timestamppb.New(c.CreatedAt),
	}
	switch c.SourceKind {
	case connector.KindExchange:
		out.Kind = casv1.ConnectionKind_CONNECTION_KIND_EXCHANGE
	case connector.KindEVM:
		out.Kind = casv1.ConnectionKind_CONNECTION_KIND_EVM_WALLET
	}
	if c.KeyFingerprint != "" {
		out.KeyFingerprint = fingerprintPrefix + c.KeyFingerprint
	}
	out.WalletAddress = c.WalletAddress
	return out
}

func sourceKind(kind string) casv1.SourceKind {
	switch kind {
	case connector.KindExchange:
		return casv1.SourceKind_SOURCE_KIND_EXCHANGE
	case connector.KindEVM:
		return casv1.SourceKind_SOURCE_KIND_EVM
	}
	return casv1.SourceKind_SOURCE_KIND_UNSPECIFIED
}

func connectionStatus(s string) casv1.ConnectionStatus {
	if v, ok := casv1.ConnectionStatus_value["CONNECTION_STATUS_"+s]; ok {
		return casv1.ConnectionStatus(v)
	}
	return casv1.ConnectionStatus_CONNECTION_STATUS_UNSPECIFIED
}

// Pagination of ListConnections (SRS — Core §2.1.1 Common rules).
const (
	defaultPageSize = 100
	maxPageSize     = 500
	pageTokenFormat = 1
	pageTokenSize   = 1 + 8 + 16
)

// GetConnection returns one connection of the tenant with the health of its streams.
func (s *connectionService) GetConnection(ctx context.Context, req *casv1.GetConnectionRequest) (*casv1.GetConnectionResponse, error) {
	id, err := parseConnectionID(req.GetConnectionId())
	if err != nil {
		return nil, err
	}
	tenantID, _ := TenantID(ctx)
	conn, streams, err := s.conns.Get(ctx, tenantID, id)
	if err != nil {
		return nil, s.errorStatus(ctx, "get connection", err)
	}
	resp := &casv1.GetConnectionResponse{Connection: toProto(conn), Streams: make([]*casv1.StreamHealth, 0, len(streams))}
	for _, h := range streams {
		out := &casv1.StreamHealth{
			Stream:              h.Stream,
			Mode:                streamMode(h.Mode),
			NextRunAt:           timestamppb.New(h.NextRunAt),
			LastError:           h.LastError,
			ConsecutiveFailures: h.ConsecutiveFailures,
		}
		if h.LastSuccessAt != nil {
			out.LastSuccessAt = timestamppb.New(*h.LastSuccessAt)
		}
		resp.Streams = append(resp.Streams, out)
	}
	return resp, nil
}

// ListConnections returns a page of the connections of the tenant in the order created_at, connection_id.
func (s *connectionService) ListConnections(ctx context.Context, req *casv1.ListConnectionsRequest) (*casv1.ListConnectionsResponse, error) {
	if utf8.RuneCountInString(req.GetOwnerRef()) > maxOwnerRef {
		return nil, status.Error(codes.InvalidArgument, "owner_ref must be at most 128 characters")
	}
	size := req.GetPageSize()
	switch {
	case size < 0:
		return nil, status.Error(codes.InvalidArgument, "page_size must not be negative")
	case size == 0:
		size = defaultPageSize
	case size > maxPageSize:
		size = maxPageSize
	}
	after, err := decodePageToken(req.GetPageToken())
	if err != nil {
		return nil, err
	}
	tenantID, _ := TenantID(ctx)
	conns, more, err := s.conns.List(ctx, tenantID, req.GetOwnerRef(), after, size)
	if err != nil {
		return nil, s.errorStatus(ctx, "list connections", err)
	}
	resp := &casv1.ListConnectionsResponse{Connections: make([]*casv1.Connection, 0, len(conns))}
	for _, c := range conns {
		resp.Connections = append(resp.Connections, toProto(c))
	}
	if more {
		last := conns[len(conns)-1]
		resp.NextPageToken = encodePageToken(registry.Position{CreatedAt: last.CreatedAt, ID: last.ID})
	}
	return resp, nil
}

// DeleteConnection deletes a connection of the tenant (SRS — Core UC-104).
func (s *connectionService) DeleteConnection(ctx context.Context, req *casv1.DeleteConnectionRequest) (*casv1.DeleteConnectionResponse, error) {
	id, err := parseConnectionID(req.GetConnectionId())
	if err != nil {
		return nil, err
	}
	tenantID, _ := TenantID(ctx)
	credentialID, _ := CredentialID(ctx)
	if err := s.conns.Delete(ctx, tenantID, credentialID, id); err != nil {
		return nil, s.errorStatus(ctx, "delete connection", err)
	}
	return &casv1.DeleteConnectionResponse{}, nil
}

// TriggerSync makes every stream of a connection of the tenant due now (FR-120).
func (s *connectionService) TriggerSync(ctx context.Context, req *casv1.TriggerSyncRequest) (*casv1.TriggerSyncResponse, error) {
	id, err := parseConnectionID(req.GetConnectionId())
	if err != nil {
		return nil, err
	}
	tenantID, _ := TenantID(ctx)
	if err := s.conns.TriggerSync(ctx, tenantID, id); err != nil {
		return nil, s.errorStatus(ctx, "trigger sync", err)
	}
	return &casv1.TriggerSyncResponse{}, nil
}

// parseConnectionID accepts a UUID in its 36-character form, either letter case.
func parseConnectionID(s string) (uuid.UUID, error) {
	id, err := uuid.Parse(s)
	if len(s) != 36 || err != nil {
		return uuid.UUID{}, status.Error(codes.InvalidArgument, "connection_id must be a UUID of 36 characters")
	}
	return id, nil
}

// encodePageToken writes the position after the last returned row: a format byte, created_at in
// microseconds since the Unix epoch, the connection ID. Opaque to the caller.
func encodePageToken(p registry.Position) string {
	b := make([]byte, 0, pageTokenSize)
	b = append(b, pageTokenFormat)
	b = binary.BigEndian.AppendUint64(b, uint64(p.CreatedAt.UnixMicro()))
	b = append(b, p.ID[:]...)
	return base64.RawURLEncoding.EncodeToString(b)
}

// decodePageToken reads a page token; empty means the first page.
func decodePageToken(token string) (registry.Position, error) {
	if token == "" {
		// Before every row: created_at is never earlier than year 1.
		return registry.Position{CreatedAt: time.Time{}}, nil
	}
	malformed := status.Error(codes.InvalidArgument, "page_token is malformed")
	b, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(b) != pageTokenSize || b[0] != pageTokenFormat {
		return registry.Position{}, malformed
	}
	createdAt := time.UnixMicro(int64(binary.BigEndian.Uint64(b[1:9]))).UTC()
	// A position outside the years 1 to 9999 was not written by encodePageToken.
	if y := createdAt.Year(); y < 1 || y > 9999 {
		return registry.Position{}, malformed
	}
	return registry.Position{CreatedAt: createdAt, ID: uuid.UUID(b[9:])}, nil
}

func streamMode(m string) casv1.StreamMode {
	if v, ok := casv1.StreamMode_value["STREAM_MODE_"+m]; ok {
		return casv1.StreamMode(v)
	}
	return casv1.StreamMode_STREAM_MODE_UNSPECIFIED
}
