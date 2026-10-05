package api

import (
	"context"
	"log/slog"
	"unicode/utf8"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/google/uuid"

	casv1 "github.com/DigitLock/crypto-account-service/internal/grpc/pb/cas/v1"
	"github.com/DigitLock/crypto-account-service/internal/registry"
)

// accountDataService reads balances and the ledger of the tenant (SRS — Core §2.1.3, §2.1.4). It reads the
// database only: no method calls a connector.
type accountDataService struct {
	casv1.UnimplementedAccountDataServiceServer
	conns  *registry.Connections
	logger *slog.Logger
}

// GetBalances returns the newest snapshot of each selected connection with its freshness.
func (s *accountDataService) GetBalances(ctx context.Context, req *casv1.GetBalancesRequest) (*casv1.GetBalancesResponse, error) {
	var ownerRef string
	var connectionID *uuid.UUID
	switch sel := req.GetSelector().(type) {
	case *casv1.GetBalancesRequest_OwnerRef:
		if sel.OwnerRef == "" || utf8.RuneCountInString(sel.OwnerRef) > maxOwnerRef {
			return nil, status.Error(codes.InvalidArgument, "owner_ref must be 1 to 128 characters")
		}
		ownerRef = sel.OwnerRef
	case *casv1.GetBalancesRequest_ConnectionId:
		id, err := parseConnectionID(sel.ConnectionId)
		if err != nil {
			return nil, err
		}
		connectionID = &id
	default:
		return nil, status.Error(codes.InvalidArgument, "a selector is required: owner_ref or connection_id")
	}

	tenantID, _ := TenantID(ctx)
	connections, balances, err := s.conns.Balances(ctx, tenantID, ownerRef, connectionID)
	if err != nil {
		return nil, s.errorStatus(ctx, "get balances", err)
	}
	resp := &casv1.GetBalancesResponse{
		Connections: make([]*casv1.ConnectionSnapshot, 0, len(connections)),
		Balances:    make([]*casv1.Balance, 0, len(balances)),
	}
	for _, c := range connections {
		out := &casv1.ConnectionSnapshot{ConnectionId: c.ConnectionID.String(), Source: c.Source, Stale: c.Stale}
		if c.AsOf != nil {
			out.AsOf = timestamppb.New(*c.AsOf)
		}
		resp.Connections = append(resp.Connections, out)
	}
	for _, b := range balances {
		resp.Balances = append(resp.Balances, &casv1.Balance{
			ConnectionId: b.ConnectionID.String(), AccountType: enumOf(casv1.AccountType_value, "ACCOUNT_TYPE_", b.AccountType, casv1.AccountType(0)),
			Asset: b.Asset, NativeAsset: b.NativeAsset, Free: b.Free, Locked: b.Locked,
		})
	}
	return resp, nil
}

// ListLedgerEntries returns entries of the tenant above after_seq in ascending seq, for incremental pull.
func (s *accountDataService) ListLedgerEntries(ctx context.Context, req *casv1.ListLedgerEntriesRequest) (*casv1.ListLedgerEntriesResponse, error) {
	invalid := func(msg string) error { return status.Error(codes.InvalidArgument, msg) }
	if req.GetAfterSeq() < 0 {
		return nil, invalid("after_seq must not be negative")
	}
	if utf8.RuneCountInString(req.GetOwnerRef()) > maxOwnerRef {
		return nil, invalid("owner_ref must be at most 128 characters")
	}
	f := registry.LedgerFilter{OwnerRef: req.GetOwnerRef()}
	if req.GetConnectionId() != "" {
		id, err := parseConnectionID(req.GetConnectionId())
		if err != nil {
			return nil, err
		}
		f.ConnectionID = &id
	}
	for _, t := range req.GetTypes() {
		name, ok := casv1.LedgerEntryType_name[int32(t)]
		if !ok || t == casv1.LedgerEntryType_LEDGER_ENTRY_TYPE_UNSPECIFIED {
			return nil, invalid("types holds an unknown or unspecified value")
		}
		f.Types = append(f.Types, name[len("LEDGER_ENTRY_TYPE_"):])
	}
	if req.GetOccurredFrom() != nil {
		from := req.GetOccurredFrom().AsTime()
		f.From = &from
	}
	if req.GetOccurredTo() != nil {
		to := req.GetOccurredTo().AsTime()
		f.To = &to
	}
	size := req.GetPageSize()
	switch {
	case size < 0:
		return nil, invalid("page_size must not be negative")
	case size == 0:
		size = defaultPageSize
	case size > maxPageSize:
		size = maxPageSize
	}

	tenantID, _ := TenantID(ctx)
	entries, more, err := s.conns.Ledger(ctx, tenantID, req.GetAfterSeq(), f, size)
	if err != nil {
		return nil, s.errorStatus(ctx, "list ledger entries", err)
	}
	resp := &casv1.ListLedgerEntriesResponse{
		Entries: make([]*casv1.LedgerEntry, 0, len(entries)), LastSeq: req.GetAfterSeq(), HasMore: more,
	}
	for _, e := range entries {
		resp.Entries = append(resp.Entries, &casv1.LedgerEntry{
			Seq:          e.Seq,
			ConnectionId: e.ConnectionID.String(),
			Type:         enumOf(casv1.LedgerEntryType_value, "LEDGER_ENTRY_TYPE_", e.Type, casv1.LedgerEntryType(0)),
			Leg:          enumOf(casv1.LedgerLeg_value, "LEDGER_LEG_", e.Leg, casv1.LedgerLeg(0)),
			Direction:    enumOf(casv1.LedgerDirection_value, "LEDGER_DIRECTION_", e.Direction, casv1.LedgerDirection(0)),
			Asset:        e.Asset,
			NativeAsset:  e.NativeAsset,
			Amount:       e.Amount,
			GroupId:      e.GroupID,
			ExternalId:   e.ExternalID,
			OccurredAt:   timestamppb.New(e.OccurredAt),
		})
		resp.LastSeq = e.Seq
	}
	return resp, nil
}

// errorStatus maps an error of the registry; INTERNAL is logged without the request.
func (s *accountDataService) errorStatus(ctx context.Context, op string, err error) error {
	return (&connectionService{logger: s.logger}).errorStatus(ctx, op, err)
}

// enumOf maps a value of the database to its enum on the wire: the enum name is the prefix (§2.1.1).
func enumOf[E ~int32](values map[string]int32, prefix, value string, unspecified E) E {
	if v, ok := values[prefix+value]; ok {
		return E(v)
	}
	return unspecified
}
