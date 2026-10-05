package main

import (
	"context"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	grpcstatus "google.golang.org/grpc/status"

	casv1 "github.com/DigitLock/crypto-account-service/internal/grpc/pb/cas/v1"
	"github.com/DigitLock/crypto-account-service/internal/testdb"
)

// S2-T307 — Req: ADR-3. The server half: server with the role cas_card_auth starts, authenticates, and fails its
// first write with INTERNAL; nothing is stored. The card-auth half is in cmd/card-auth.
func TestT307_RoleSeparation(t *testing.T) {
	env, callCtx, _ := databaseEnv(t)
	env["DATABASE_URL"] = testdb.CardAuthURL(t)
	owner := testdb.Open(t)
	r := startServer(t, env)

	_, err := r.client.CreateConnection(callCtx, &casv1.CreateConnectionRequest{
		OwnerRef: "owner-1", Source: "anvil",
		Credential: &casv1.CreateConnectionRequest_Wallet{Wallet: &casv1.Wallet{Address: "0x" + randomHex(t, 40)}},
	})
	if grpcstatus.Code(err) != codes.Internal {
		t.Errorf("CreateConnection as cas_card_auth: %v, want INTERNAL", err)
	}

	// A wallet connection inserted by the owner role: RegisterCard writes cards, which cas_card_auth cannot.
	var connectionID string
	if err := owner.QueryRow(context.Background(), `INSERT INTO connections (tenant_id, owner_ref, source_id, external_account)
		SELECT t.id, 'owner-1', s.id, '0xwallet' FROM tenants t, sources s WHERE s.code = 'anvil' RETURNING id::text`).
		Scan(&connectionID); err != nil {
		t.Fatal(err)
	}
	conn, err := grpc.NewClient("127.0.0.1:"+env["GRPC_PORT"], grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_, err = casv1.NewCardServiceClient(conn).RegisterCard(callCtx, &casv1.RegisterCardRequest{
		CardRef: "card-1", OwnerRef: "owner-1", ConnectionId: connectionID, DailyLimit: "1",
	})
	if grpcstatus.Code(err) != codes.Internal {
		t.Errorf("RegisterCard as cas_card_auth: %v, want INTERNAL", err)
	}

	var stored int
	if err := owner.QueryRow(context.Background(), `SELECT (SELECT count(*) FROM connections) - 1
		+ (SELECT count(*) FROM cards) + (SELECT count(*) FROM audit_log WHERE action <> 'TENANT_CREATED'
		AND action <> 'CREDENTIAL_ISSUED')`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != 0 {
		t.Errorf("%d rows stored by server as cas_card_auth, want none", stored)
	}
	r.stop()
}
