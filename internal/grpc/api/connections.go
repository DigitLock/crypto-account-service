package api

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"unicode/utf8"

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
		return nil, s.createError(ctx, err)
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

// createError maps an error of registry.Create to its status.
func (s *connectionService) createError(ctx context.Context, err error) error {
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
	case ctx.Err() != nil:
		return status.FromContextError(ctx.Err()).Err()
	default:
		return s.internal(ctx, "create connection", err)
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
