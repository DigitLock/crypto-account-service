// Phase 3 of docs/test-plan-s2.md, CardService rows T308 – T318: the server of cmd/server in process, with the
// real interceptor and the pool of cas_server. Authorizations, events and returns are inserted by the owner role:
// card-auth writes them from st5.
package api_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/DigitLock/crypto-account-service/internal/connector/evm"
	"github.com/DigitLock/crypto-account-service/internal/grpc/api"
	casv1 "github.com/DigitLock/crypto-account-service/internal/grpc/pb/cas/v1"
	"github.com/DigitLock/crypto-account-service/internal/registry"
)

func cardClient(conn *grpc.ClientConn) casv1.CardServiceClient {
	return casv1.NewCardServiceClient(conn)
}

// wallet creates a wallet connection on anvil and returns its ID and EIP-55 address.
func wallet(t *testing.T, conn *grpc.ClientConn, who caller, owner string) (string, string) {
	t.Helper()
	address := newAddress(t)
	resp, err := client(conn).CreateConnection(who.ctx, walletRequest(owner, "anvil", address))
	if err != nil {
		t.Fatal(err)
	}
	return resp.GetConnection().GetConnectionId(), evm.ToEIP55(address[2:])
}

func register(t *testing.T, conn *grpc.ClientConn, who caller, ref, owner, connectionID, limit string) *casv1.Card {
	t.Helper()
	resp, err := cardClient(conn).RegisterCard(who.ctx, &casv1.RegisterCardRequest{
		CardRef: ref, OwnerRef: owner, ConnectionId: connectionID, DailyLimit: limit,
	})
	if err != nil {
		t.Fatalf("RegisterCard %s: %v", ref, err)
	}
	return resp.GetCard()
}

// cardAudit returns the details of the audit rows of a card with an action, oldest first, and checks the
// acting credential of each.
func cardAudit(t *testing.T, e *env, action, cardRef string, credentialID uuid.UUID) []map[string]any {
	t.Helper()
	rows, err := e.owner.Query(ctx, `SELECT credential_id, details FROM audit_log
		WHERE action = $1 AND object_id = $2 ORDER BY id`, action, cardRef)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var cred uuid.UUID
		var raw []byte
		if err := rows.Scan(&cred, &raw); err != nil {
			t.Fatal(err)
		}
		if cred != credentialID {
			t.Errorf("%s of %s: credential %v, want %v", action, cardRef, cred, credentialID)
		}
		var d map[string]any
		if err := json.Unmarshal(raw, &d); err != nil {
			t.Fatal(err)
		}
		out = append(out, d)
	}
	return out
}

// S2-T308 — Req: FR-115, FR-117, SRS — Core §2.1.5
func TestT308_RegisterCard(t *testing.T) {
	e := setup(t)
	a := e.caller(t, "tenant-a")
	conn := e.realServer(t)
	connID, address := wallet(t, conn, a, "user-4821")

	card := register(t, conn, a, "card_7Q2M", "user-4821", connID, "200000000")
	want := &casv1.Card{
		CardRef: "card_7Q2M", OwnerRef: "user-4821", ConnectionId: connID, WalletAddress: address,
		Status: casv1.CardStatus_CARD_STATUS_ACTIVE, DailyLimit: "200000000",
		CreatedAt: timestamppb.New(testNow), UpdatedAt: timestamppb.New(testNow),
	}
	if !proto.Equal(card, want) {
		t.Errorf("card = %v\nwant %v", card, want)
	}

	var status, limit string
	if err := e.owner.QueryRow(ctx, `SELECT status, daily_limit::text FROM cards WHERE tenant_id = $1 AND card_ref = $2`,
		a.tenantID, "card_7Q2M").Scan(&status, &limit); err != nil {
		t.Fatal(err)
	}
	if status != "ACTIVE" || limit != "200000000" {
		t.Errorf("row: %s %s", status, limit)
	}
	audits := cardAudit(t, e, "CARD_REGISTERED", "card_7Q2M", a.credentialID)
	wantDetails := map[string]any{"owner_ref": "user-4821", "connection_id": connID, "daily_limit": "200000000"}
	if len(audits) != 1 || fmt.Sprint(audits[0]) != fmt.Sprint(wantDetails) {
		t.Errorf("audit = %v, want one row %v", audits, wantDetails)
	}

	t.Run("leading zeros are dropped", func(t *testing.T) {
		if c := register(t, conn, a, "card-zero", "user-4821", connID, "000"); c.GetDailyLimit() != "0" {
			t.Errorf("daily_limit = %q, want 0", c.GetDailyLimit())
		}
	})
}

// S2-T309 — Req: SRS — Core §2.1.5
func TestT309_RegisterCardRepeated(t *testing.T) {
	e := setup(t)
	a := e.caller(t, "tenant-a")
	conn := e.realServer(t)
	connID, _ := wallet(t, conn, a, "owner-1")
	otherID, _ := wallet(t, conn, a, "owner-1")
	first := register(t, conn, a, "card-1", "owner-1", connID, "1000")

	e.now = e.now.Add(time.Hour)
	again := register(t, conn, a, "card-1", "owner-1", connID, "01000")
	if !proto.Equal(again, first) {
		t.Errorf("repeated request = %v, want the existing card %v", again, first)
	}

	c := cardClient(conn)
	for name, req := range map[string]*casv1.RegisterCardRequest{
		"another daily_limit": {CardRef: "card-1", OwnerRef: "owner-1", ConnectionId: connID, DailyLimit: "2000"},
		"another connection":  {CardRef: "card-1", OwnerRef: "owner-1", ConnectionId: otherID, DailyLimit: "1000"},
		"another owner":       {CardRef: "card-1", OwnerRef: "owner-2", ConnectionId: connID, DailyLimit: "1000"},
	} {
		_, err := c.RegisterCard(a.ctx, req)
		assertCode(t, name, err, codes.AlreadyExists)
	}
	if n := count(t, e, `SELECT count(*) FROM cards`); n != 1 {
		t.Errorf("cards = %d, want 1", n)
	}
	if audits := cardAudit(t, e, "CARD_REGISTERED", "card-1", a.credentialID); len(audits) != 1 {
		t.Errorf("CARD_REGISTERED rows = %d, want 1", len(audits))
	}

	t.Run("concurrent requests with the same body", func(t *testing.T) {
		var wg sync.WaitGroup
		errs := make([]error, 8)
		for i := range errs {
			wg.Go(func() {
				_, errs[i] = c.RegisterCard(a.ctx, &casv1.RegisterCardRequest{
					CardRef: "card-2", OwnerRef: "owner-1", ConnectionId: connID, DailyLimit: "5",
				})
			})
		}
		wg.Wait()
		for _, err := range errs {
			assertCode(t, "concurrent same body", err, codes.OK)
		}
		if audits := cardAudit(t, e, "CARD_REGISTERED", "card-2", a.credentialID); len(audits) != 1 {
			t.Errorf("CARD_REGISTERED rows = %d, want 1", len(audits))
		}
	})
}

// S2-T310 — Req: EC-113, owner's decision D-12
func TestT310_ConnectionNotUsable(t *testing.T) {
	e := setup(t)
	a := e.caller(t, "tenant-a")
	conn := e.realServer(t)
	exchangeID, _ := createExchange(t, client(conn), a, "owner-1")
	invalidID, _ := wallet(t, conn, a, "owner-1")
	degradedID, _ := wallet(t, conn, a, "owner-1")
	otherOwnerID, _ := wallet(t, conn, a, "owner-2")
	for id, st := range map[string]string{invalidID: "CREDENTIALS_INVALID", degradedID: "DEGRADED"} {
		if _, err := e.owner.Exec(ctx, `UPDATE connections SET status = $1 WHERE id = $2`, st, id); err != nil {
			t.Fatal(err)
		}
	}

	for name, id := range map[string]string{
		"exchange connection": exchangeID, "CREDENTIALS_INVALID wallet": invalidID,
		"wallet of another owner": otherOwnerID,
	} {
		_, err := cardClient(conn).RegisterCard(a.ctx, &casv1.RegisterCardRequest{
			CardRef: "card-1", OwnerRef: "owner-1", ConnectionId: id, DailyLimit: "1",
		})
		assertReason(t, name, err, codes.FailedPrecondition, "cas/CONNECTION_NOT_USABLE")
	}
	if n := count(t, e, `SELECT count(*) FROM cards`) + count(t, e, `SELECT count(*) FROM audit_log WHERE action = 'CARD_REGISTERED'`); n != 0 {
		t.Errorf("%d card or audit rows stored, want none", n)
	}

	// D-12: DEGRADED is a label only, the wallet is usable.
	if card := register(t, conn, a, "card-degraded", "owner-1", degradedID, "1"); card.GetConnectionId() != degradedID {
		t.Errorf("card on the DEGRADED wallet = %v", card)
	}
}

// S2-T311 — Req: SRS — Core §2.1.1, §2.1.5
func TestT311_Validation(t *testing.T) {
	e := setup(t)
	a := e.caller(t, "tenant-a")
	conn := e.realServer(t)
	connID, _ := wallet(t, conn, a, "owner-1")
	register(t, conn, a, "card-1", "owner-1", connID, "1")
	c := cardClient(conn)

	valid := func() *casv1.RegisterCardRequest {
		return &casv1.RegisterCardRequest{CardRef: "card-new", OwnerRef: "owner-1", ConnectionId: connID, DailyLimit: "1"}
	}
	for name, change := range map[string]func(r *casv1.RegisterCardRequest){
		"card_ref empty":             func(r *casv1.RegisterCardRequest) { r.CardRef = "" },
		"card_ref 65 characters":     func(r *casv1.RegisterCardRequest) { r.CardRef = strings.Repeat("ж", 65) },
		"owner_ref empty":            func(r *casv1.RegisterCardRequest) { r.OwnerRef = "" },
		"owner_ref 129 characters":   func(r *casv1.RegisterCardRequest) { r.OwnerRef = strings.Repeat("o", 129) },
		"daily_limit empty":          func(r *casv1.RegisterCardRequest) { r.DailyLimit = "" },
		"daily_limit -1":             func(r *casv1.RegisterCardRequest) { r.DailyLimit = "-1" },
		"daily_limit +1":             func(r *casv1.RegisterCardRequest) { r.DailyLimit = "+1" },
		"daily_limit 1.5":            func(r *casv1.RegisterCardRequest) { r.DailyLimit = "1.5" },
		"daily_limit 1e6":            func(r *casv1.RegisterCardRequest) { r.DailyLimit = "1e6" },
		"daily_limit with spaces":    func(r *casv1.RegisterCardRequest) { r.DailyLimit = " 1" },
		"daily_limit 79 digits":      func(r *casv1.RegisterCardRequest) { r.DailyLimit = strings.Repeat("9", 79) },
		"connection_id malformed":    func(r *casv1.RegisterCardRequest) { r.ConnectionId = "5b7d2c90" },
		"connection_id without dash": func(r *casv1.RegisterCardRequest) { r.ConnectionId = strings.ReplaceAll(connID, "-", "") },
	} {
		req := valid()
		change(req)
		_, err := c.RegisterCard(a.ctx, req)
		assertCode(t, "RegisterCard "+name, err, codes.InvalidArgument)
	}
	t.Run("78 digits is accepted", func(t *testing.T) {
		req := valid()
		req.DailyLimit = strings.Repeat("9", 78)
		if _, err := c.RegisterCard(a.ctx, req); err != nil {
			t.Error(err)
		}
	})
	_, err := c.RegisterCard(a.ctx, &casv1.RegisterCardRequest{
		CardRef: "card-x", OwnerRef: "owner-1", ConnectionId: uuid.NewString(), DailyLimit: "1",
	})
	assertCode(t, "unknown connection_id", err, codes.NotFound)

	limit := "5"
	for name, call := range map[string]func() error{
		"UpdateCard without fields": func() error {
			_, err := c.UpdateCard(a.ctx, &casv1.UpdateCardRequest{CardRef: "card-1"})
			return err
		},
		"UpdateCard status UNSPECIFIED": func() error {
			st := casv1.CardStatus_CARD_STATUS_UNSPECIFIED
			_, err := c.UpdateCard(a.ctx, &casv1.UpdateCardRequest{CardRef: "card-1", Status: &st})
			return err
		},
		"UpdateCard unknown status": func() error {
			st := casv1.CardStatus(7)
			_, err := c.UpdateCard(a.ctx, &casv1.UpdateCardRequest{CardRef: "card-1", Status: &st})
			return err
		},
		"UpdateCard daily_limit 1.5": func() error {
			bad := "1.5"
			_, err := c.UpdateCard(a.ctx, &casv1.UpdateCardRequest{CardRef: "card-1", DailyLimit: &bad})
			return err
		},
		"UpdateCard card_ref empty": func() error {
			_, err := c.UpdateCard(a.ctx, &casv1.UpdateCardRequest{DailyLimit: &limit})
			return err
		},
		"GetCard card_ref empty": func() error {
			_, err := c.GetCard(a.ctx, &casv1.GetCardRequest{})
			return err
		},
		"ListCards negative page_size": func() error {
			_, err := c.ListCards(a.ctx, &casv1.ListCardsRequest{PageSize: -1})
			return err
		},
		"ListCards owner_ref 129": func() error {
			_, err := c.ListCards(a.ctx, &casv1.ListCardsRequest{OwnerRef: strings.Repeat("o", 129)})
			return err
		},
		"ListCards malformed page_token": func() error {
			_, err := c.ListCards(a.ctx, &casv1.ListCardsRequest{PageToken: "not-a-token"})
			return err
		},
		"GetAuthorization auth_id empty": func() error {
			_, err := c.GetAuthorization(a.ctx, &casv1.GetAuthorizationRequest{})
			return err
		},
		"GetAuthorization auth_id 65": func() error {
			_, err := c.GetAuthorization(a.ctx, &casv1.GetAuthorizationRequest{AuthId: strings.Repeat("a", 65)})
			return err
		},
		"ListAuthorizations negative page_size": func() error {
			_, err := c.ListAuthorizations(a.ctx, &casv1.ListAuthorizationsRequest{PageSize: -1})
			return err
		},
		"ListAuthorizations card token as page_token": func() error {
			resp, err := c.ListCards(a.ctx, &casv1.ListCardsRequest{PageSize: 1})
			if err != nil || resp.GetNextPageToken() == "" {
				return fmt.Errorf("no card page token: %v", err)
			}
			_, err = c.ListAuthorizations(a.ctx, &casv1.ListAuthorizationsRequest{PageToken: resp.GetNextPageToken()})
			return err
		},
	} {
		assertCode(t, name, call(), codes.InvalidArgument)
	}
	_, err = c.GetCard(a.ctx, &casv1.GetCardRequest{CardRef: "card-unknown"})
	assertCode(t, "GetCard unknown", err, codes.NotFound)
	_, err = c.UpdateCard(a.ctx, &casv1.UpdateCardRequest{CardRef: "card-unknown", DailyLimit: &limit})
	assertCode(t, "UpdateCard unknown", err, codes.NotFound)
}

// S2-T312 — Req: FR-115, FR-117, UC-103
func TestT312_UpdateCard(t *testing.T) {
	e := setup(t)
	a := e.caller(t, "tenant-a")
	conn := e.realServer(t)
	connID, _ := wallet(t, conn, a, "owner-1")
	register(t, conn, a, "card-1", "owner-1", connID, "1000")
	c := cardClient(conn)
	frozen, active := casv1.CardStatus_CARD_STATUS_FROZEN, casv1.CardStatus_CARD_STATUS_ACTIVE

	update := func(t *testing.T, req *casv1.UpdateCardRequest) *casv1.Card {
		t.Helper()
		req.CardRef = "card-1"
		resp, err := c.UpdateCard(a.ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		return resp.GetCard()
	}
	limit := "2500"
	steps := []struct {
		name    string
		req     *casv1.UpdateCardRequest
		status  casv1.CardStatus
		limit   string
		changed map[string]any // nil: nothing changes
	}{
		{"change the limit", &casv1.UpdateCardRequest{DailyLimit: &limit}, active, "2500", map[string]any{"daily_limit": "2500"}},
		{"freeze", &casv1.UpdateCardRequest{Status: &frozen}, frozen, "2500", map[string]any{"status": "FROZEN"}},
		{"freeze again", &casv1.UpdateCardRequest{Status: &frozen}, frozen, "2500", nil},
		{"same limit with leading zeros", &casv1.UpdateCardRequest{DailyLimit: proto.String("02500")}, frozen, "2500", nil},
		{"unfreeze and change the limit", &casv1.UpdateCardRequest{Status: &active, DailyLimit: proto.String("0")}, active, "0",
			map[string]any{"status": "ACTIVE", "daily_limit": "0"}},
	}
	var wantAudits []map[string]any
	lastUpdate := testNow
	for _, s := range steps {
		e.now = e.now.Add(time.Minute)
		card := update(t, s.req)
		if s.changed != nil {
			wantAudits = append(wantAudits, s.changed)
			lastUpdate = e.now
		}
		if card.GetStatus() != s.status || card.GetDailyLimit() != s.limit || !card.GetUpdatedAt().AsTime().Equal(lastUpdate) ||
			!card.GetCreatedAt().AsTime().Equal(testNow) {
			t.Errorf("%s: card = %v; want %v, %s, updated_at %v", s.name, card, s.status, s.limit, lastUpdate)
		}
	}
	if got := cardAudit(t, e, "CARD_UPDATED", "card-1", a.credentialID); fmt.Sprint(got) != fmt.Sprint(wantAudits) {
		t.Errorf("CARD_UPDATED rows = %v, want %v", got, wantAudits)
	}
	var stored string
	if err := e.owner.QueryRow(ctx, `SELECT status || ' ' || daily_limit::text FROM cards`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != "ACTIVE 0" {
		t.Errorf("row = %s", stored)
	}
}

// S2-T313 — Req: FR-115
func TestT313_GetAndListCards(t *testing.T) {
	e := setup(t)
	a := e.caller(t, "tenant-a")
	conn := e.realServer(t)
	conn1, addr1 := wallet(t, conn, a, "owner-1")
	conn2, _ := wallet(t, conn, a, "owner-2")
	c := cardClient(conn)

	// card-b and card-a share created_at: card_ref orders them.
	register(t, conn, a, "card-b", "owner-1", conn1, "1")
	register(t, conn, a, "card-a", "owner-1", conn1, "2")
	e.now = e.now.Add(time.Second)
	register(t, conn, a, "card-0", "owner-2", conn2, "3")

	got, err := c.GetCard(a.ctx, &casv1.GetCardRequest{CardRef: "card-b"})
	if err != nil {
		t.Fatal(err)
	}
	if card := got.GetCard(); card.GetWalletAddress() != addr1 || card.GetOwnerRef() != "owner-1" ||
		card.GetConnectionId() != conn1 || card.GetDailyLimit() != "1" || card.GetStatus() != casv1.CardStatus_CARD_STATUS_ACTIVE {
		t.Errorf("GetCard = %v", card)
	}

	refs := func(req *casv1.ListCardsRequest) []string {
		var out []string
		for {
			resp, err := c.ListCards(a.ctx, req)
			if err != nil {
				t.Fatal(err)
			}
			for _, card := range resp.GetCards() {
				out = append(out, card.GetCardRef())
			}
			if resp.GetNextPageToken() == "" {
				return out
			}
			req.PageToken = resp.GetNextPageToken()
		}
	}
	for name, tc := range map[string]struct {
		req  *casv1.ListCardsRequest
		want []string
	}{
		"all":              {&casv1.ListCardsRequest{}, []string{"card-a", "card-b", "card-0"}},
		"all, pages of 1":  {&casv1.ListCardsRequest{PageSize: 1}, []string{"card-a", "card-b", "card-0"}},
		"owner-1":          {&casv1.ListCardsRequest{OwnerRef: "owner-1"}, []string{"card-a", "card-b"}},
		"owner-1, pages":   {&casv1.ListCardsRequest{OwnerRef: "owner-1", PageSize: 1}, []string{"card-a", "card-b"}},
		"unknown owner":    {&casv1.ListCardsRequest{OwnerRef: "owner-9"}, nil},
		"page size of 600": {&casv1.ListCardsRequest{PageSize: 600}, []string{"card-a", "card-b", "card-0"}},
	} {
		if got := refs(tc.req); !slices.Equal(got, tc.want) {
			t.Errorf("%s: %v, want %v", name, got, tc.want)
		}
	}
	resp, err := c.ListCards(a.ctx, &casv1.ListCardsRequest{PageSize: 3})
	if err != nil || len(resp.GetCards()) != 3 || resp.GetNextPageToken() != "" {
		t.Errorf("a full last page: %v, token %q, want no token", err, resp.GetNextPageToken())
	}
	_, err = c.GetCard(a.ctx, &casv1.GetCardRequest{CardRef: "card-x"})
	assertCode(t, "unknown card_ref", err, codes.NotFound)
}

// S2-T314 — Req: FR-105, BR-13
func TestT314_TenantIsolationOfCards(t *testing.T) {
	e := setup(t)
	a := e.caller(t, "tenant-a")
	b := e.caller(t, "tenant-b")
	conn := e.realServer(t)
	connA, _ := wallet(t, conn, a, "owner-1")
	connB, _ := wallet(t, conn, b, "owner-1")
	register(t, conn, a, "card-1", "owner-1", connA, "1")
	register(t, conn, b, "card-b", "owner-1", connB, "1")
	c := cardClient(conn)
	before := cardRows(t, e)

	_, err := c.GetCard(b.ctx, &casv1.GetCardRequest{CardRef: "card-1"})
	assertCode(t, "GetCard of A as B", err, codes.NotFound)
	frozen := casv1.CardStatus_CARD_STATUS_FROZEN
	_, err = c.UpdateCard(b.ctx, &casv1.UpdateCardRequest{CardRef: "card-1", Status: &frozen})
	assertCode(t, "UpdateCard of A as B", err, codes.NotFound)
	_, err = c.RegisterCard(b.ctx, &casv1.RegisterCardRequest{CardRef: "card-x", OwnerRef: "owner-1", ConnectionId: connA, DailyLimit: "1"})
	assertCode(t, "RegisterCard on A's connection as B", err, codes.NotFound)
	resp, err := c.ListCards(b.ctx, &casv1.ListCardsRequest{})
	if err != nil || len(resp.GetCards()) != 1 || resp.GetCards()[0].GetCardRef() != "card-b" {
		t.Errorf("ListCards as B = %v, %v; want only card-b", resp.GetCards(), err)
	}
	if after := cardRows(t, e); after != before {
		t.Errorf("cards changed:\nbefore %s\nafter  %s", before, after)
	}
	// The same card_ref in another tenant is another card.
	register(t, conn, b, "card-1", "owner-1", connB, "7")
}

func cardRows(t *testing.T, e *env) string {
	t.Helper()
	var s string
	if err := e.owner.QueryRow(ctx, `SELECT coalesce(string_agg(k::text, ';' ORDER BY k.id), '') FROM cards k`).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

// raceCardDB registers a card on the connection, through the owner role and committed, right before the delete
// statement of the delete transaction: the card check of the transaction has already passed.
type raceCardDB struct {
	registry.DB
	e        *env
	tenantID uuid.UUID
}

func (d raceCardDB) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := d.DB.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return raceCardTx{tx, d}, nil
}

type raceCardTx struct {
	pgx.Tx
	d raceCardDB
}

func (tx raceCardTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if strings.Contains(sql, "DELETE FROM connections") {
		if _, err := tx.d.e.owner.Exec(ctx, `INSERT INTO cards (tenant_id, card_ref, owner_ref, connection_id, daily_limit)
			VALUES ($1, 'card-late', 'owner-1', $2, 1)`, tx.d.tenantID, args[0]); err != nil {
			return pgconn.CommandTag{}, err
		}
	}
	return tx.Tx.Exec(ctx, sql, args...)
}

// S2-T315 — Req: EC-114, SRS — Core UC-104
func TestT315_DeleteConnectionWithCards(t *testing.T) {
	e := setup(t)
	a := e.caller(t, "tenant-a")
	conn := e.realServer(t)
	withCard, _ := wallet(t, conn, a, "owner-1")
	without, _ := wallet(t, conn, a, "owner-1")
	register(t, conn, a, "card-1", "owner-1", withCard, "1")

	_, err := client(conn).DeleteConnection(a.ctx, &casv1.DeleteConnectionRequest{ConnectionId: withCard})
	assertReason(t, "delete with a card", err, codes.FailedPrecondition, "cas/CONNECTION_HAS_CARDS")
	if n := count(t, e, `SELECT count(*) FROM connections WHERE id = $1`, withCard); n != 1 {
		t.Error("the connection with a card was deleted")
	}
	if n := count(t, e, `SELECT count(*) FROM audit_log WHERE action = 'CONNECTION_DELETED'`); n != 0 {
		t.Errorf("CONNECTION_DELETED rows = %d, want none", n)
	}

	if _, err := client(conn).DeleteConnection(a.ctx, &casv1.DeleteConnectionRequest{ConnectionId: without}); err != nil {
		t.Errorf("delete without cards: %v", err)
	}

	t.Run("a card registered after the check is not orphaned", func(t *testing.T) {
		racing, _ := wallet(t, conn, a, "owner-1")
		raceConn := dial(t, api.NewServer(e.deps(raceCardDB{DB: e.server, e: e, tenantID: a.tenantID})))
		_, err := client(raceConn).DeleteConnection(a.ctx, &casv1.DeleteConnectionRequest{ConnectionId: racing})
		assertReason(t, "delete racing a card", err, codes.FailedPrecondition, "cas/CONNECTION_HAS_CARDS")
		if n := count(t, e, `SELECT count(*) FROM cards k JOIN connections c ON c.id = k.connection_id
			WHERE k.card_ref = 'card-late'`); n != 1 {
			t.Error("the late card has no connection")
		}
		if n := count(t, e, `SELECT count(*) FROM audit_log WHERE action = 'CONNECTION_DELETED'`); n != 1 {
			t.Errorf("CONNECTION_DELETED rows = %d, want only the one of the connection without cards", n)
		}
	})
}

// S2-T316 — Req: SRS — Core §2.1.5
func TestT316_SeveralCardsOnOneWallet(t *testing.T) {
	e := setup(t)
	a := e.caller(t, "tenant-a")
	conn := e.realServer(t)
	connID, address := wallet(t, conn, a, "owner-1")
	one := register(t, conn, a, "card-1", "owner-1", connID, "100")
	two := register(t, conn, a, "card-2", "owner-1", connID, "200")
	if one.GetWalletAddress() != address || two.GetWalletAddress() != address || one.GetConnectionId() != two.GetConnectionId() {
		t.Errorf("cards = %v, %v; want both on %s", one, two, address)
	}
	if n := count(t, e, `SELECT count(*) FROM cards WHERE connection_id = $1`, connID); n != 2 {
		t.Errorf("cards on the wallet = %d, want 2", n)
	}
}

// authRow is an authorization inserted by the owner role, as card-auth will write it.
type authRow struct {
	authID, status, reason string
	cardID                 *uuid.UUID
	receivedAt             time.Time
}

func insertAuthorization(t *testing.T, e *env, tenantID uuid.UUID, r authRow, extra string, args ...any) uuid.UUID {
	t.Helper()
	var reason *string
	if r.reason != "" {
		reason = &r.reason
	}
	// A stand-in for keccak256(tenant_id, auth_id): 32 bytes, unique per tenant and auth_id.
	chainAuthID := sha256.Sum256([]byte(tenantID.String() + r.authID))
	all := append([]any{tenantID, r.authID, chainAuthID[:], r.cardID, r.status, reason, r.receivedAt}, args...)
	var id uuid.UUID
	if err := e.owner.QueryRow(ctx, `INSERT INTO authorizations (tenant_id, auth_id, chain_auth_id, card_id, status,
		decline_reason, received_at`+extra+`) VALUES ($1, $2, $3, $4, $5, $6, $7`+
		placeholders(8, len(args))+`) RETURNING id`, all...).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// txHashBytes is a 32-byte transaction hash derived from a label of the test.
func txHashBytes(label string) []byte {
	h := sha256.Sum256([]byte(label))
	return h[:]
}

// txHash is txHashBytes as the API returns it: 0x and 64 hexadecimal characters.
func txHash(label string) string { return "0x" + hex.EncodeToString(txHashBytes(label)) }

func placeholders(from, n int) string {
	var b strings.Builder
	for i := range n {
		fmt.Fprintf(&b, ", $%d", from+i)
	}
	return b.String()
}

func cardID(t *testing.T, e *env, tenantID uuid.UUID, ref string) *uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := e.owner.QueryRow(ctx, `SELECT id FROM cards WHERE tenant_id = $1 AND card_ref = $2`, tenantID, ref).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return &id
}

// S2-T317 — Req: FR-116, FR-105, SRS — Core §2.1.1. The approved authorization with returns of T502 and the
// tombstone of T507, inserted directly; served with the role cas_server.
func TestT317_GetAuthorization(t *testing.T) {
	e := setup(t)
	a := e.caller(t, "tenant-a")
	b := e.caller(t, "tenant-b")
	conn := e.realServer(t)
	connID, _ := wallet(t, conn, a, "owner-1")
	register(t, conn, a, "card_7Q2M", "owner-1", connID, "200000000")
	at := func(ms int) time.Time {
		return time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC).Add(time.Duration(ms) * time.Millisecond)
	}

	// Approved, final, two returns: the authorization of T502.
	id := insertAuthorization(t, e, a.tenantID, authRow{
		authID: "9f1c2a7e-5b1d-4c58-9a57-0d2f6f1e8a11", status: "DEBIT_CONFIRMED",
		cardID: cardID(t, e, a.tenantID, "card_7Q2M"), receivedAt: at(120),
	}, `, fiat_amount, fiat_currency, rate, buffer_bps, token, token_amount, debited_amount, returned_amount, decided_at`,
		"25.40", "EUR", "1.1642", 100, "USDC", "29866387", "29866387", "29866387", at(910))
	for i, ev := range []struct {
		from, to string
		ms       int
	}{{"", "RECEIVED", 120}, {"RECEIVED", "DEBIT_SUBMITTED", 480}, {"DEBIT_SUBMITTED", "APPROVED", 910}, {"APPROVED", "DEBIT_CONFIRMED", 1188300}} {
		var from *string
		if ev.from != "" {
			from = &ev.from
		}
		if _, err := e.owner.Exec(ctx, `INSERT INTO authorization_events (authorization_id, from_status, to_status, created_at)
			VALUES ($1, $2, $3, $4)`, id, from, ev.to, at(ev.ms)); err != nil {
			t.Fatalf("event %d: %v", i, err)
		}
	}
	returnRows := map[string]uuid.UUID{}
	for i, r := range []struct {
		id, amount string
		ms         int
	}{{"rv-20261002-0002", "18107967", 5000}, {"rv-20261002-0001", "11758420", 3000}} {
		var rowID uuid.UUID
		if err := e.owner.QueryRow(ctx, `INSERT INTO returns (tenant_id, authorization_id, return_id, chain_refund_id, type,
			token_amount, status, created_at) VALUES ($1, $2, $3, sha256(convert_to($3, 'UTF8')), 'REVERSAL', $4, 'CONFIRMED', $5)
			RETURNING id`, a.tenantID, id, r.id, r.amount, at(r.ms)).Scan(&rowID); err != nil {
			t.Fatalf("return %d: %v", i, err)
		}
		returnRows[r.id] = rowID
	}

	// Operator transactions (D-11). The debit was resubmitted: the first DEBIT row reverted, the second is included.
	// Return 0001 has one REFUND row; return 0002 was resent: the replacement is SENT, newer than the CONFIRMED one.
	nonce := 0
	opTx := func(purpose string, authorizationID, returnRowID *uuid.UUID, status string, hash string, ms int) {
		t.Helper()
		var raw []byte
		if hash != "" {
			raw = txHashBytes(hash)
		}
		if _, err := e.owner.Exec(ctx, `INSERT INTO operator_txs (chain_id, operator_address, nonce, purpose,
			authorization_id, return_row_id, tx_hash, status, created_at) VALUES (31337, $1, $2, $3, $4, $5, $6, $7, $8)`,
			make([]byte, 20), nonce, purpose, authorizationID, returnRowID, raw, status, at(ms)); err != nil {
			t.Fatal(err)
		}
		nonce++
	}
	rv1, rv2 := returnRows["rv-20261002-0001"], returnRows["rv-20261002-0002"]
	opTx("DEBIT", &id, nil, "REVERTED", "debit-reverted", 200)
	opTx("DEBIT", &id, nil, "INCLUDED", "debit-included", 400)
	opTx("REFUND", nil, &rv1, "CONFIRMED", "refund-1", 3100)
	opTx("REFUND", nil, &rv2, "CONFIRMED", "refund-2-confirmed", 5100)
	opTx("REFUND", nil, &rv2, "SENT", "refund-2-sent", 5200)

	// The rule over the DEBIT rows of three more authorizations.
	priority := insertAuthorization(t, e, a.tenantID, authRow{authID: "auth-priority", status: "APPROVED", receivedAt: at(1)}, "")
	opTx("DEBIT", &priority, nil, "INCLUDED", "priority-included", 10)
	opTx("DEBIT", &priority, nil, "SENT", "priority-newer-sent", 20)
	sent := insertAuthorization(t, e, a.tenantID, authRow{authID: "auth-sent", status: "DEBIT_SUBMITTED", receivedAt: at(2)}, "")
	opTx("DEBIT", &sent, nil, "SENT", "sent", 30)
	opTx("DEBIT", &sent, nil, "PLANNED", "", 40)
	planned := insertAuthorization(t, e, a.tenantID, authRow{authID: "auth-planned", status: "RECEIVED", receivedAt: at(3)}, "")
	opTx("DEBIT", &planned, nil, "PLANNED", "", 50)

	// Tombstone: a reversal arrived first (T507).
	tomb := insertAuthorization(t, e, a.tenantID, authRow{
		authID: "auth-tombstone", status: "DECLINED", reason: "REVERSED_BEFORE_AUTH", receivedAt: at(0),
	}, "")
	if _, err := e.owner.Exec(ctx, `INSERT INTO authorization_events (authorization_id, to_status, reason, created_at)
		VALUES ($1, 'DECLINED', 'REVERSED_BEFORE_AUTH', $2)`, tomb, at(0)); err != nil {
		t.Fatal(err)
	}
	if _, err := e.owner.Exec(ctx, `INSERT INTO returns (tenant_id, authorization_id, return_id, chain_refund_id, type,
		token_amount, status, created_at) VALUES ($1, $2, 'rv-first', sha256('rv-first'), 'REVERSAL', 0, 'NOTHING_TO_RETURN', $3)`,
		a.tenantID, tomb, at(0)); err != nil {
		t.Fatal(err)
	}

	c := cardClient(conn)
	get := func(who caller, authID string) (*casv1.Authorization, error) {
		resp, err := c.GetAuthorization(who.ctx, &casv1.GetAuthorizationRequest{AuthId: authID})
		return resp.GetAuthorization(), err
	}

	got, err := get(a, "9f1c2a7e-5b1d-4c58-9a57-0d2f6f1e8a11")
	if err != nil {
		t.Fatal(err)
	}
	want := &casv1.Authorization{
		AuthId: "9f1c2a7e-5b1d-4c58-9a57-0d2f6f1e8a11", Status: casv1.AuthorizationStatus_AUTHORIZATION_STATUS_DEBIT_CONFIRMED,
		Amount: "25.4", Currency: "EUR", Token: "USDC", TokenAmount: "29866387",
		Quote:         &casv1.Quote{Rate: "1.1642", BufferBps: 100},
		DebitedAmount: "29866387", ReturnedAmount: "29866387", CardRef: "card_7Q2M",
		ReceivedAt: timestamppb.New(at(120)), DecidedAt: timestamppb.New(at(910)),
		TxHash: txHash("debit-included"),
		Returns: []*casv1.Return{
			{ReturnId: "rv-20261002-0001", Type: casv1.ReturnType_RETURN_TYPE_REVERSAL, Status: casv1.ReturnStatus_RETURN_STATUS_CONFIRMED,
				TokenAmount: "11758420", CreatedAt: timestamppb.New(at(3000)), TxHash: txHash("refund-1")},
			{ReturnId: "rv-20261002-0002", Type: casv1.ReturnType_RETURN_TYPE_REVERSAL, Status: casv1.ReturnStatus_RETURN_STATUS_CONFIRMED,
				TokenAmount: "18107967", CreatedAt: timestamppb.New(at(5000)), TxHash: txHash("refund-2-confirmed")},
		},
		History: []*casv1.StatusChange{
			{Status: casv1.AuthorizationStatus_AUTHORIZATION_STATUS_RECEIVED, At: timestamppb.New(at(120))},
			{Status: casv1.AuthorizationStatus_AUTHORIZATION_STATUS_DEBIT_SUBMITTED, At: timestamppb.New(at(480))},
			{Status: casv1.AuthorizationStatus_AUTHORIZATION_STATUS_APPROVED, At: timestamppb.New(at(910))},
			{Status: casv1.AuthorizationStatus_AUTHORIZATION_STATUS_DEBIT_CONFIRMED, At: timestamppb.New(at(1188300))},
		},
	}
	if !proto.Equal(got, want) {
		t.Errorf("authorization =\n%v\nwant\n%v", got, want)
	}

	tombGot, err := get(a, "auth-tombstone")
	if err != nil {
		t.Fatal(err)
	}
	wantTomb := &casv1.Authorization{
		AuthId: "auth-tombstone", Status: casv1.AuthorizationStatus_AUTHORIZATION_STATUS_DECLINED,
		DeclineReason: casv1.DeclineReason_DECLINE_REASON_REVERSED_BEFORE_AUTH, DebitedAmount: "0", ReturnedAmount: "0",
		ReceivedAt: timestamppb.New(at(0)),
		Returns: []*casv1.Return{{ReturnId: "rv-first", Type: casv1.ReturnType_RETURN_TYPE_REVERSAL,
			Status: casv1.ReturnStatus_RETURN_STATUS_NOTHING_TO_RETURN, TokenAmount: "0", CreatedAt: timestamppb.New(at(0))}},
		History: []*casv1.StatusChange{{Status: casv1.AuthorizationStatus_AUTHORIZATION_STATUS_DECLINED,
			Reason: "REVERSED_BEFORE_AUTH", At: timestamppb.New(at(0))}},
	}
	if !proto.Equal(tombGot, wantTomb) {
		t.Errorf("tombstone =\n%v\nwant\n%v", tombGot, wantTomb)
	}

	t.Run("tx_hash by the rule, in GetAuthorization and in ListAuthorizations", func(t *testing.T) {
		want := map[string]string{
			"9f1c2a7e-5b1d-4c58-9a57-0d2f6f1e8a11": txHash("debit-included"),
			"auth-priority":                        txHash("priority-included"),
			"auth-sent":                            txHash("sent"),
			"auth-planned":                         "",
			"auth-tombstone":                       "",
		}
		resp, err := c.ListAuthorizations(a.ctx, &casv1.ListAuthorizationsRequest{})
		if err != nil {
			t.Fatal(err)
		}
		listed := map[string]string{}
		for _, item := range resp.GetAuthorizations() {
			listed[item.GetAuthId()] = item.GetTxHash()
		}
		for authID, hash := range want {
			one, err := get(a, authID)
			if err != nil {
				t.Fatal(err)
			}
			if one.GetTxHash() != hash || listed[authID] != hash {
				t.Errorf("%s: tx_hash %q, listed %q; want %q", authID, one.GetTxHash(), listed[authID], hash)
			}
		}
	})

	_, err = get(a, "auth-unknown")
	assertCode(t, "unknown auth_id", err, codes.NotFound)
	_, err = get(b, "9f1c2a7e-5b1d-4c58-9a57-0d2f6f1e8a11")
	assertCode(t, "A's auth_id as B", err, codes.NotFound)
}

// S2-T318 — Req: FR-116, FR-105, SRS — Core §2.1.1
func TestT318_ListAuthorizations(t *testing.T) {
	e := setup(t)
	a := e.caller(t, "tenant-a")
	b := e.caller(t, "tenant-b")
	conn := e.realServer(t)
	conn1, _ := wallet(t, conn, a, "owner-1")
	conn2, _ := wallet(t, conn, a, "owner-2")
	register(t, conn, a, "card-1", "owner-1", conn1, "1")
	register(t, conn, a, "card-2", "owner-2", conn2, "1")
	cards := map[string]*uuid.UUID{"card-1": cardID(t, e, a.tenantID, "card-1"), "card-2": cardID(t, e, a.tenantID, "card-2")}
	owners := map[string]string{"card-1": "owner-1", "card-2": "owner-2"}
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)

	type row struct {
		authID, card, status string
		receivedAt           time.Time
	}
	statuses := []string{"APPROVED", "DECLINED", "DEBIT_CONFIRMED"}
	var rows []row
	for i := range 30 {
		// Pairs share received_at, so auth_id breaks the tie; the IDs are not in time order.
		r := row{authID: fmt.Sprintf("auth-%02d", (i*7)%30), card: []string{"card-1", "card-2"}[i%2],
			status: statuses[i%3], receivedAt: base.Add(time.Duration(i/2) * time.Hour)}
		rows = append(rows, r)
		insertAuthorization(t, e, a.tenantID, authRow{authID: r.authID, status: r.status, cardID: cards[r.card], receivedAt: r.receivedAt}, "")
	}
	for i, id := range []string{"tomb-1", "tomb-2"} {
		r := row{authID: id, status: "DECLINED", receivedAt: base.Add(time.Duration(i*5) * time.Hour)}
		rows = append(rows, r)
		insertAuthorization(t, e, a.tenantID, authRow{authID: id, status: "DECLINED", reason: "REVERSED_BEFORE_AUTH", receivedAt: r.receivedAt}, "")
	}
	insertAuthorization(t, e, b.tenantID, authRow{authID: "auth-b", status: "APPROVED", receivedAt: base}, "")
	sort.Slice(rows, func(i, j int) bool {
		if !rows[i].receivedAt.Equal(rows[j].receivedAt) {
			return rows[i].receivedAt.After(rows[j].receivedAt)
		}
		return rows[i].authID < rows[j].authID
	})

	c := cardClient(conn)
	list := func(who caller, req *casv1.ListAuthorizationsRequest) []string {
		t.Helper()
		var out []string
		for {
			resp, err := c.ListAuthorizations(who.ctx, req)
			if err != nil {
				t.Fatal(err)
			}
			for _, item := range resp.GetAuthorizations() {
				if len(item.GetReturns()) != 0 || len(item.GetHistory()) != 0 {
					t.Errorf("%s carries returns or history", item.GetAuthId())
				}
				out = append(out, item.GetAuthId())
			}
			if resp.GetNextPageToken() == "" {
				return out
			}
			req.PageToken = resp.GetNextPageToken()
		}
	}
	expect := func(keep func(r row) bool) []string {
		var out []string
		for _, r := range rows {
			if keep(r) {
				out = append(out, r.authID)
			}
		}
		return out
	}
	from, to := base.Add(3*time.Hour), base.Add(9*time.Hour)
	inRange := func(r row) bool { return !r.receivedAt.Before(from) && r.receivedAt.Before(to) }

	for name, tc := range map[string]struct {
		req  *casv1.ListAuthorizationsRequest
		keep func(r row) bool
	}{
		"all":                          {&casv1.ListAuthorizationsRequest{}, func(row) bool { return true }},
		"all, pages":                   {&casv1.ListAuthorizationsRequest{PageSize: 7}, func(row) bool { return true }},
		"by card_ref":                  {&casv1.ListAuthorizationsRequest{CardRef: "card-1", PageSize: 4}, func(r row) bool { return r.card == "card-1" }},
		"by owner_ref":                 {&casv1.ListAuthorizationsRequest{OwnerRef: "owner-2"}, func(r row) bool { return owners[r.card] == "owner-2" }},
		"by status":                    {&casv1.ListAuthorizationsRequest{Status: casv1.AuthorizationStatus_AUTHORIZATION_STATUS_DECLINED}, func(r row) bool { return r.status == "DECLINED" }},
		"from inclusive, to exclusive": {&casv1.ListAuthorizationsRequest{ReceivedFrom: timestamppb.New(from), ReceivedTo: timestamppb.New(to), PageSize: 3}, inRange},
		"combined": {&casv1.ListAuthorizationsRequest{CardRef: "card-2", Status: casv1.AuthorizationStatus_AUTHORIZATION_STATUS_APPROVED,
			ReceivedFrom: timestamppb.New(from), ReceivedTo: timestamppb.New(to)},
			func(r row) bool { return r.card == "card-2" && r.status == "APPROVED" && inRange(r) }},
		"unknown card": {&casv1.ListAuthorizationsRequest{CardRef: "card-9"}, func(row) bool { return false }},
	} {
		got, want := list(a, tc.req), expect(tc.keep)
		if !slices.Equal(got, want) {
			t.Errorf("%s:\n got %v\nwant %v", name, got, want)
		}
	}
	if all := list(a, &casv1.ListAuthorizationsRequest{PageSize: 5}); len(all) != 32 || !slices.Contains(all, "tomb-1") {
		t.Errorf("pages hold %d rows, want every row once, tombstones included", len(all))
	}
	_, err := c.ListAuthorizations(a.ctx, &casv1.ListAuthorizationsRequest{Status: casv1.AuthorizationStatus(42)})
	assertCode(t, "unknown status", err, codes.InvalidArgument)
	if got := list(b, &casv1.ListAuthorizationsRequest{}); !slices.Equal(got, []string{"auth-b"}) {
		t.Errorf("as B: %v, want only auth-b", got)
	}
}
