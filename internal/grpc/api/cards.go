package api

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	casv1 "github.com/DigitLock/crypto-account-service/internal/grpc/pb/cas/v1"
	"github.com/DigitLock/crypto-account-service/internal/registry"
)

// Rules of CardService (SRS — Core §2.1.1, §2.1.5).
const (
	reasonConnectionNotUsable = "CONNECTION_NOT_USABLE"
	reasonConnectionHasCards  = "CONNECTION_HAS_CARDS"
	maxCardRef, maxAuthID     = 64, 64

	cardTokenFormat          = 2
	authorizationTokenFormat = 3
)

// baseUnits is a base-unit integer string: digits only, at most 78 (SRS — Core §2.1.5).
var baseUnits = regexp.MustCompile(`^[0-9]{1,78}$`)

// cardService implements CardService. It reads and writes the card registry and reads the authorizations.
type cardService struct {
	casv1.UnimplementedCardServiceServer
	cards  *registry.Cards
	logger *slog.Logger
}

// RegisterCard binds a card_ref to a wallet connection of the tenant (SRS — Core §2.1.5).
func (s *cardService) RegisterCard(ctx context.Context, req *casv1.RegisterCardRequest) (*casv1.RegisterCardResponse, error) {
	if err := validCardRef(req.GetCardRef()); err != nil {
		return nil, err
	}
	switch n := utf8.RuneCountInString(req.GetOwnerRef()); {
	case n == 0:
		return nil, status.Error(codes.InvalidArgument, "owner_ref is required")
	case n > maxOwnerRef:
		return nil, status.Error(codes.InvalidArgument, "owner_ref must be at most 128 characters")
	}
	limit, err := dailyLimit(req.GetDailyLimit())
	if err != nil {
		return nil, err
	}
	connectionID, err := parseConnectionID(req.GetConnectionId())
	if err != nil {
		return nil, err
	}
	tenantID, _ := TenantID(ctx)
	credentialID, _ := CredentialID(ctx)
	card, err := s.cards.Register(ctx, registry.RegisterInput{
		TenantID: tenantID, CredentialID: credentialID, CardRef: req.GetCardRef(), OwnerRef: req.GetOwnerRef(),
		ConnectionID: connectionID, DailyLimit: limit,
	})
	if err != nil {
		return nil, s.errorStatus(ctx, "register card", err)
	}
	return &casv1.RegisterCardResponse{Card: cardToProto(card)}, nil
}

// UpdateCard changes the daily limit or the status of a card of the tenant (SRS — Core UC-103).
func (s *cardService) UpdateCard(ctx context.Context, req *casv1.UpdateCardRequest) (*casv1.UpdateCardResponse, error) {
	if err := validCardRef(req.GetCardRef()); err != nil {
		return nil, err
	}
	if req.DailyLimit == nil && req.Status == nil {
		return nil, status.Error(codes.InvalidArgument, "daily_limit or status is required")
	}
	var limit, cardStatus *string
	if req.DailyLimit != nil {
		v, err := dailyLimit(req.GetDailyLimit())
		if err != nil {
			return nil, err
		}
		limit = &v
	}
	if req.Status != nil {
		var v string
		switch req.GetStatus() {
		case casv1.CardStatus_CARD_STATUS_ACTIVE:
			v = registry.CardActive
		case casv1.CardStatus_CARD_STATUS_FROZEN:
			v = registry.CardFrozen
		default:
			return nil, status.Error(codes.InvalidArgument, "status must be ACTIVE or FROZEN")
		}
		cardStatus = &v
	}
	tenantID, _ := TenantID(ctx)
	credentialID, _ := CredentialID(ctx)
	card, err := s.cards.Update(ctx, tenantID, credentialID, req.GetCardRef(), limit, cardStatus)
	if err != nil {
		return nil, s.errorStatus(ctx, "update card", err)
	}
	return &casv1.UpdateCardResponse{Card: cardToProto(card)}, nil
}

// GetCard returns a card of the tenant.
func (s *cardService) GetCard(ctx context.Context, req *casv1.GetCardRequest) (*casv1.GetCardResponse, error) {
	if err := validCardRef(req.GetCardRef()); err != nil {
		return nil, err
	}
	tenantID, _ := TenantID(ctx)
	card, err := s.cards.Get(ctx, tenantID, req.GetCardRef())
	if err != nil {
		return nil, s.errorStatus(ctx, "get card", err)
	}
	return &casv1.GetCardResponse{Card: cardToProto(card)}, nil
}

// ListCards returns a page of the cards of the tenant in the order created_at, card_ref.
func (s *cardService) ListCards(ctx context.Context, req *casv1.ListCardsRequest) (*casv1.ListCardsResponse, error) {
	if utf8.RuneCountInString(req.GetOwnerRef()) > maxOwnerRef {
		return nil, status.Error(codes.InvalidArgument, "owner_ref must be at most 128 characters")
	}
	size, err := pageSize(req.GetPageSize())
	if err != nil {
		return nil, err
	}
	var after registry.CardPosition
	if req.GetPageToken() != "" {
		at, ref, err := decodeKeyToken(req.GetPageToken(), cardTokenFormat)
		if err != nil {
			return nil, err
		}
		after = registry.CardPosition{CreatedAt: at, CardRef: ref}
	}
	tenantID, _ := TenantID(ctx)
	cards, more, err := s.cards.List(ctx, tenantID, req.GetOwnerRef(), after, size)
	if err != nil {
		return nil, s.errorStatus(ctx, "list cards", err)
	}
	resp := &casv1.ListCardsResponse{Cards: make([]*casv1.Card, 0, len(cards))}
	for _, c := range cards {
		resp.Cards = append(resp.Cards, cardToProto(c))
	}
	if more {
		last := cards[len(cards)-1]
		resp.NextPageToken = encodeKeyToken(cardTokenFormat, last.CreatedAt, last.CardRef)
	}
	return resp, nil
}

// GetAuthorization returns an authorization of the tenant with its returns and history.
func (s *cardService) GetAuthorization(ctx context.Context, req *casv1.GetAuthorizationRequest) (*casv1.GetAuthorizationResponse, error) {
	if n := utf8.RuneCountInString(req.GetAuthId()); n == 0 || n > maxAuthID {
		return nil, status.Error(codes.InvalidArgument, "auth_id must be 1 to 64 characters")
	}
	tenantID, _ := TenantID(ctx)
	a, err := s.cards.Authorization(ctx, tenantID, req.GetAuthId())
	if err != nil {
		return nil, s.errorStatus(ctx, "get authorization", err)
	}
	return &casv1.GetAuthorizationResponse{Authorization: authorizationToProto(a)}, nil
}

// ListAuthorizations returns a page of the authorizations of the tenant, newest first, without returns and
// history. Filters combine with AND; received_from is inclusive, received_to exclusive.
func (s *cardService) ListAuthorizations(ctx context.Context, req *casv1.ListAuthorizationsRequest) (*casv1.ListAuthorizationsResponse, error) {
	switch {
	case utf8.RuneCountInString(req.GetCardRef()) > maxCardRef:
		return nil, status.Error(codes.InvalidArgument, "card_ref must be at most 64 characters")
	case utf8.RuneCountInString(req.GetOwnerRef()) > maxOwnerRef:
		return nil, status.Error(codes.InvalidArgument, "owner_ref must be at most 128 characters")
	}
	f := registry.AuthorizationFilter{CardRef: req.GetCardRef(), OwnerRef: req.GetOwnerRef()}
	if st := req.GetStatus(); st != casv1.AuthorizationStatus_AUTHORIZATION_STATUS_UNSPECIFIED {
		name, ok := casv1.AuthorizationStatus_name[int32(st)]
		if !ok {
			return nil, status.Error(codes.InvalidArgument, "status is not a known authorization status")
		}
		f.Status = strings.TrimPrefix(name, "AUTHORIZATION_STATUS_")
	}
	for _, t := range []struct {
		name string
		ts   *timestamppb.Timestamp
		dst  **time.Time
	}{{"received_from", req.GetReceivedFrom(), &f.From}, {"received_to", req.GetReceivedTo(), &f.To}} {
		if t.ts == nil {
			continue
		}
		if err := t.ts.CheckValid(); err != nil {
			return nil, status.Error(codes.InvalidArgument, t.name+" is not a valid timestamp")
		}
		v := t.ts.AsTime()
		*t.dst = &v
	}
	size, err := pageSize(req.GetPageSize())
	if err != nil {
		return nil, err
	}
	var after *registry.AuthorizationPosition
	if req.GetPageToken() != "" {
		at, id, err := decodeKeyToken(req.GetPageToken(), authorizationTokenFormat)
		if err != nil {
			return nil, err
		}
		after = &registry.AuthorizationPosition{ReceivedAt: at, AuthID: id}
	}
	tenantID, _ := TenantID(ctx)
	list, more, err := s.cards.Authorizations(ctx, tenantID, f, after, size)
	if err != nil {
		return nil, s.errorStatus(ctx, "list authorizations", err)
	}
	resp := &casv1.ListAuthorizationsResponse{Authorizations: make([]*casv1.Authorization, 0, len(list))}
	for _, a := range list {
		resp.Authorizations = append(resp.Authorizations, authorizationToProto(a))
	}
	if more {
		last := list[len(list)-1]
		resp.NextPageToken = encodeKeyToken(authorizationTokenFormat, last.ReceivedAt, last.AuthID)
	}
	return resp, nil
}

// GetReconciliationReport returns a reconciliation run of the tenant on a source: the run run_id, or the newest
// (SRS — Core §2.1.1; S3 D-10, S3 D-24). A run of another tenant or source is NOT_FOUND, as no run at all.
func (s *cardService) GetReconciliationReport(ctx context.Context, req *casv1.GetReconciliationReportRequest) (*casv1.GetReconciliationReportResponse, error) {
	if req.GetSource() == "" {
		return nil, status.Error(codes.InvalidArgument, "source is required")
	}
	var runID *uuid.UUID
	if raw := req.GetRunId(); raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil || len(raw) != 36 {
			return nil, status.Error(codes.InvalidArgument, "run_id must be a UUID in its 36-character form")
		}
		runID = &id
	}
	tenantID, _ := TenantID(ctx)
	run, err := s.cards.ReconciliationRun(ctx, tenantID, req.GetSource(), runID)
	if err != nil {
		return nil, s.errorStatus(ctx, "get reconciliation report", err)
	}
	return &casv1.GetReconciliationReportResponse{Run: runToProto(run)}, nil
}

func runToProto(r registry.ReconciliationRun) *casv1.ReconciliationRun {
	out := &casv1.ReconciliationRun{
		RunId: r.ID.String(), Source: r.Source, PeriodFrom: timestamppb.New(r.PeriodFrom), PeriodTo: timestamppb.New(r.PeriodTo),
		ToBlock: strconv.FormatInt(r.ToBlock, 10), CreatedAt: timestamppb.New(r.CreatedAt),
		Totals: &casv1.ReconciliationTotals{
			AuthorizationsChecked: r.Totals.AuthorizationsChecked, DebitsCount: r.Totals.DebitsCount,
			DebitsAmount: r.Totals.DebitsAmount, ReturnsChecked: r.Totals.ReturnsChecked,
			RefundsCount: r.Totals.RefundsCount, RefundsAmount: r.Totals.RefundsAmount,
		},
		Mismatches: make([]*casv1.ReconciliationMismatch, 0, len(r.Mismatches)),
	}
	for _, m := range r.Mismatches {
		out.Mismatches = append(out.Mismatches, &casv1.ReconciliationMismatch{
			Type:   casv1.ReconciliationMismatchType(casv1.ReconciliationMismatchType_value["RECONCILIATION_MISMATCH_TYPE_"+m.Type]),
			AuthId: m.AuthID, ReturnId: m.ReturnID, ChainAuthId: m.ChainAuthID, ChainRefundId: m.ChainRefundID,
			TxHash: m.TxHash, ExpectedAmount: m.ExpectedAmount, ActualAmount: m.ActualAmount, Asset: m.Asset,
			CheckpointBalance: m.CheckpointBalance, LedgerTotal: m.LedgerTotal,
		})
	}
	return out
}

// errorStatus maps an error of the card registry to its status.
func (s *cardService) errorStatus(ctx context.Context, op string, err error) error {
	switch {
	case errors.Is(err, registry.ErrConnectionNotFound):
		return status.Error(codes.NotFound, "connection not found")
	case errors.Is(err, registry.ErrConnectionNotUsable):
		return withReason(codes.FailedPrecondition,
			"the connection is not a wallet, not ACTIVE or DEGRADED, or belongs to another owner", reasonConnectionNotUsable)
	case errors.Is(err, registry.ErrCardExists):
		return status.Error(codes.AlreadyExists, "the card is already registered with different data")
	case errors.Is(err, registry.ErrCardNotFound):
		return status.Error(codes.NotFound, "card not found")
	case errors.Is(err, registry.ErrAuthorizationNotFound):
		return status.Error(codes.NotFound, "authorization not found")
	case errors.Is(err, registry.ErrSourceNotFound):
		return status.Error(codes.NotFound, "source not found")
	case errors.Is(err, registry.ErrRunNotFound):
		return status.Error(codes.NotFound, "reconciliation run not found")
	case ctx.Err() != nil:
		return status.FromContextError(ctx.Err()).Err()
	default:
		s.logger.ErrorContext(ctx, op+" failed", "error", err)
		return status.Error(codes.Internal, msgInternal)
	}
}

func validCardRef(ref string) error {
	if n := utf8.RuneCountInString(ref); n == 0 || n > maxCardRef {
		return status.Error(codes.InvalidArgument, "card_ref must be 1 to 64 characters")
	}
	return nil
}

// dailyLimit checks a base-unit integer string and returns it without leading zeros.
func dailyLimit(v string) (string, error) {
	if !baseUnits.MatchString(v) {
		return "", status.Error(codes.InvalidArgument,
			"daily_limit must be a base-unit integer string: digits only, at most 78, no sign, point or exponent")
	}
	if v = strings.TrimLeft(v, "0"); v == "" {
		v = "0"
	}
	return v, nil
}

// pageSize applies the pagination rule of SRS — Core §2.1.1.
func pageSize(size int32) (int32, error) {
	switch {
	case size < 0:
		return 0, status.Error(codes.InvalidArgument, "page_size must not be negative")
	case size == 0:
		return defaultPageSize, nil
	case size > maxPageSize:
		return maxPageSize, nil
	}
	return size, nil
}

// encodeKeyToken writes a position after the last returned row: a format byte, a time in microseconds since the
// Unix epoch, and a key. Opaque to the caller; the format byte keeps the tokens of the lists apart.
func encodeKeyToken(format byte, at time.Time, key string) string {
	b := make([]byte, 0, 9+len(key))
	b = append(b, format)
	b = binary.BigEndian.AppendUint64(b, uint64(at.UnixMicro()))
	b = append(b, key...)
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeKeyToken(token string, format byte) (time.Time, string, error) {
	malformed := status.Error(codes.InvalidArgument, "page_token is malformed")
	b, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(b) < 10 || b[0] != format || !utf8.Valid(b[9:]) {
		return time.Time{}, "", malformed
	}
	at := time.UnixMicro(int64(binary.BigEndian.Uint64(b[1:9]))).UTC()
	if y := at.Year(); y < 1 || y > 9999 {
		return time.Time{}, "", malformed
	}
	return at, string(b[9:]), nil
}

func cardToProto(c registry.Card) *casv1.Card {
	st := casv1.CardStatus_CARD_STATUS_UNSPECIFIED
	if v, ok := casv1.CardStatus_value["CARD_STATUS_"+c.Status]; ok {
		st = casv1.CardStatus(v)
	}
	return &casv1.Card{
		CardRef: c.CardRef, OwnerRef: c.OwnerRef, ConnectionId: c.ConnectionID.String(), WalletAddress: c.WalletAddress,
		Status: st, DailyLimit: c.DailyLimit, CreatedAt: timestamppb.New(c.CreatedAt), UpdatedAt: timestamppb.New(c.UpdatedAt),
	}
}

func authorizationToProto(a registry.Authorization) *casv1.Authorization {
	out := &casv1.Authorization{
		AuthId:         a.AuthID,
		Status:         casv1.AuthorizationStatus(casv1.AuthorizationStatus_value["AUTHORIZATION_STATUS_"+a.Status]),
		Amount:         a.Amount,
		Currency:       a.Currency,
		Token:          a.Token,
		TokenAmount:    a.TokenAmount,
		DebitedAmount:  a.DebitedAmount,
		ReturnedAmount: a.ReturnedAmount,
		TxHash:         a.TxHash,
		CardRef:        a.CardRef,
		ReceivedAt:     timestamppb.New(a.ReceivedAt),
	}
	if a.DeclineReason != "" {
		out.DeclineReason = casv1.DeclineReason(casv1.DeclineReason_value["DECLINE_REASON_"+a.DeclineReason])
	}
	if a.BufferBps != nil {
		out.Quote = &casv1.Quote{Rate: a.Rate, BufferBps: *a.BufferBps}
	}
	if a.DecidedAt != nil {
		out.DecidedAt = timestamppb.New(*a.DecidedAt)
	}
	for _, r := range a.Returns {
		out.Returns = append(out.Returns, &casv1.Return{
			ReturnId:    r.ReturnID,
			Type:        casv1.ReturnType(casv1.ReturnType_value["RETURN_TYPE_"+r.Type]),
			Status:      casv1.ReturnStatus(casv1.ReturnStatus_value["RETURN_STATUS_"+r.Status]),
			TokenAmount: r.TokenAmount,
			TxHash:      r.TxHash,
			CreatedAt:   timestamppb.New(r.CreatedAt),
		})
	}
	for _, h := range a.History {
		out.History = append(out.History, &casv1.StatusChange{
			Status: casv1.AuthorizationStatus(casv1.AuthorizationStatus_value["AUTHORIZATION_STATUS_"+h.Status]),
			Reason: h.Reason,
			At:     timestamppb.New(h.At),
		})
	}
	return out
}
