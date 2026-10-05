// Phase 4 of docs/test-plan-c1.md, authentication rows: a gRPC server in process (bufconn) with the real
// interceptor. The server side uses the pool of TEST_DATABASE_URL_SERVER, as cas_server; the registry,
// which casctl drives, uses the owner role.
package api_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"

	"github.com/DigitLock/crypto-account-service/internal/auth"
	"github.com/DigitLock/crypto-account-service/internal/grpc/api"
	casv1 "github.com/DigitLock/crypto-account-service/internal/grpc/pb/cas/v1"
	"github.com/DigitLock/crypto-account-service/internal/registry"
	"github.com/DigitLock/crypto-account-service/internal/repository"
	"github.com/DigitLock/crypto-account-service/internal/testdb"
)

var ctx = context.Background()

// syncBuffer is a log sink shared by the server goroutines and the test.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type env struct {
	owner  *pgxpool.Pool
	server *pgxpool.Pool
	reg    *registry.Registry
	log    *syncBuffer
	logger *slog.Logger
	tokens []string
}

func setup(t *testing.T) *env {
	t.Helper()
	e := &env{owner: testdb.Open(t), log: &syncBuffer{}}
	testdb.Clean(t)
	e.server = testdb.OpenServer(t)
	e.reg = registry.New(e.owner)
	e.logger = slog.New(slog.NewJSONHandler(e.log, &slog.HandlerOptions{Level: slog.LevelDebug}))
	// The token secrets of the test appear nowhere in the captured log.
	t.Cleanup(func() {
		out := e.log.String()
		for _, tok := range e.tokens {
			if strings.Contains(out, tok) || strings.Contains(out, tok[len("cas_")+13:]) {
				t.Error("the captured log contains a token secret")
			}
		}
		if strings.Contains(strings.ToLower(out), "authorization") || strings.Contains(out, "Bearer") {
			t.Error("the captured log contains request metadata")
		}
	})
	return e
}

func (e *env) tenant(t *testing.T, name string) registry.Tenant {
	t.Helper()
	tn, err := e.reg.CreateTenant(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	return tn
}

func (e *env) issue(t *testing.T, tenant string) registry.IssuedToken {
	t.Helper()
	tok, err := e.reg.IssueToken(ctx, tenant)
	if err != nil {
		t.Fatal(err)
	}
	e.tokens = append(e.tokens, tok.Value)
	return tok
}

// dial serves srv on an in-memory listener and returns a client connection.
func dial(t *testing.T, srv *grpc.Server) *grpc.ClientConn {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	go func() { _ = srv.Serve(lis) }()
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = conn.Close()
		srv.Stop()
	})
	return conn
}

// realServer is the server of cmd/server: api.NewServer with the queries of cas_server.
func (e *env) realServer(t *testing.T) *grpc.ClientConn {
	t.Helper()
	return dial(t, api.NewServer(repository.New(e.server), e.logger))
}

func bearer(token string) context.Context {
	return metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token)
}

func listSources(conn *grpc.ClientConn, callCtx context.Context) error {
	_, err := casv1.NewConnectionServiceClient(conn).ListSources(callCtx, &casv1.ListSourcesRequest{})
	return err
}

func assertCode(t *testing.T, what string, err error, want codes.Code) {
	t.Helper()
	if got := status.Code(err); got != want {
		t.Errorf("%s: %v, want %v", what, err, want)
	}
}

// recorder is a test handler behind the interceptor: it records the identity of the context.
type recorder struct {
	casv1.UnimplementedConnectionServiceServer
	tenantID, credentialID uuid.UUID
	ok                     bool
}

func (r *recorder) ListSources(ctx context.Context, _ *casv1.ListSourcesRequest) (*casv1.ListSourcesResponse, error) {
	var ok1, ok2 bool
	r.tenantID, ok1 = api.TenantID(ctx)
	r.credentialID, ok2 = api.CredentialID(ctx)
	r.ok = ok1 && ok2
	return &casv1.ListSourcesResponse{}, nil
}

// C1-T403 — Req: §2.1.1
func TestT403_ValidToken(t *testing.T) {
	e := setup(t)
	tn := e.tenant(t, "tenant-a")
	tok := e.issue(t, "tenant-a")
	var credentialID uuid.UUID
	if err := e.owner.QueryRow(ctx, `SELECT id FROM api_credentials WHERE key_id = $1`, tok.KeyID).
		Scan(&credentialID); err != nil {
		t.Fatal(err)
	}

	t.Run("handler sees the tenant and the credential", func(t *testing.T) {
		rec := &recorder{}
		srv := grpc.NewServer(grpc.UnaryInterceptor(api.UnaryAuth(repository.New(e.server), e.logger)))
		casv1.RegisterConnectionServiceServer(srv, rec)
		if err := listSources(dial(t, srv), bearer(tok.Value)); err != nil {
			t.Fatal(err)
		}
		if !rec.ok || rec.tenantID != tn.ID || rec.credentialID != credentialID {
			t.Errorf("context: tenant %v, credential %v, set %v; want %v, %v", rec.tenantID, rec.credentialID,
				rec.ok, tn.ID, credentialID)
		}
	})

	t.Run("real registration answers UNIMPLEMENTED", func(t *testing.T) {
		assertCode(t, "ListSources with a valid token", listSources(e.realServer(t), bearer(tok.Value)),
			codes.Unimplemented)
	})
}

// failingStore fails like a database that went away.
type failingStore struct{}

func (failingStore) GetServiceTokenByKeyID(context.Context, string) (repository.GetServiceTokenByKeyIDRow, error) {
	return repository.GetServiceTokenByKeyIDRow{}, errors.New("driver: connection reset by peer")
}

// C1-T404 — Req: §2.1.1
func TestT404_BadToken(t *testing.T) {
	e := setup(t)
	e.tenant(t, "tenant-a")
	tok := e.issue(t, "tenant-a")
	conn := e.realServer(t)

	unknown, _, _, err := auth.New()
	if err != nil {
		t.Fatal(err)
	}
	// The key_id of a stored token with another secret.
	wrongSecret := "cas_" + tok.KeyID + "_" + strings.Repeat("0", 64)
	e.tokens = append(e.tokens, unknown)

	cases := map[string]context.Context{
		"no metadata":        ctx,
		"without Bearer":     metadata.AppendToOutgoingContext(ctx, "authorization", tok.Value),
		"unknown key_id":     bearer(unknown),
		"wrong secret":       bearer(wrongSecret),
		"malformed token":    bearer("cas_not-a-token"),
		"other scheme":       metadata.AppendToOutgoingContext(ctx, "authorization", "Basic "+tok.Value),
		"two authorizations": metadata.AppendToOutgoingContext(bearer(tok.Value), "authorization", "Bearer x"),
	}
	var messages []string
	for name, callCtx := range cases {
		err := listSources(conn, callCtx)
		assertCode(t, name, err, codes.Unauthenticated)
		messages = append(messages, status.Convert(err).Message())
	}
	for _, m := range messages {
		if m != messages[0] {
			t.Errorf("messages differ: %q", messages)
			break
		}
	}

	t.Run("database failure is INTERNAL without the driver error", func(t *testing.T) {
		failing := dial(t, api.NewServer(failingStore{}, e.logger))
		err := listSources(failing, bearer(tok.Value))
		assertCode(t, "store failure", err, codes.Internal)
		if strings.Contains(status.Convert(err).Message(), "driver") {
			t.Errorf("message %q exposes the driver error", status.Convert(err).Message())
		}
	})
}

// C1-T405 — Req: UC-105
func TestT405_RevokedToken(t *testing.T) {
	e := setup(t)
	e.tenant(t, "tenant-a")
	tok := e.issue(t, "tenant-a")
	conn := e.realServer(t)

	assertCode(t, "before revoke", listSources(conn, bearer(tok.Value)), codes.Unimplemented)
	if _, err := e.reg.RevokeToken(ctx, tok.KeyID); err != nil {
		t.Fatal(err)
	}
	assertCode(t, "after revoke", listSources(conn, bearer(tok.Value)), codes.Unauthenticated)
}

// C1-T406 — Req: UC-105, EC-121
func TestT406_DisabledTenant(t *testing.T) {
	e := setup(t)
	e.tenant(t, "tenant-a")
	tok := e.issue(t, "tenant-a")
	conn := e.realServer(t)
	before := tenantRows(t, e.owner)

	if _, err := e.reg.DisableTenant(ctx, "tenant-a"); err != nil {
		t.Fatal(err)
	}
	disabledErr := listSources(conn, bearer(tok.Value))
	assertCode(t, "while disabled", disabledErr, codes.Unauthenticated)
	unknown, _, _, _ := auth.New()
	if a, b := status.Convert(disabledErr).Message(), status.Convert(listSources(conn, bearer(unknown))).Message(); a != b {
		t.Errorf("disabled tenant message %q differs from unknown token %q", a, b)
	}

	if _, err := e.reg.EnableTenant(ctx, "tenant-a"); err != nil {
		t.Fatal(err)
	}
	assertCode(t, "after enable", listSources(conn, bearer(tok.Value)), codes.Unimplemented)
	if after := tenantRows(t, e.owner); after != before {
		t.Errorf("rows of the tenant changed:\nbefore %s\nafter  %s", before, after)
	}
}

// tenantRows returns the tenant and credential rows as text.
func tenantRows(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var s string
	if err := pool.QueryRow(ctx, `SELECT (SELECT string_agg(t::text, ';' ORDER BY t.id) FROM tenants t)
		|| '|' || (SELECT string_agg(c::text, ';' ORDER BY c.id) FROM api_credentials c)`).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

// C1-T407 — Req: FR-105. Every method of every service of cas.v1, read from the descriptors, so that a
// method added later is covered.
func TestT407_EveryMethodNeedsAToken(t *testing.T) {
	e := setup(t)
	e.tenant(t, "tenant-a")
	tok := e.issue(t, "tenant-a")
	conn := e.realServer(t)

	_ = casv1.File_cas_v1_connection_service_proto
	var methods []protoreflect.MethodDescriptor
	protoregistry.GlobalFiles.RangeFilesByPackage("cas.v1", func(fd protoreflect.FileDescriptor) bool {
		for i := range fd.Services().Len() {
			sd := fd.Services().Get(i)
			for j := range sd.Methods().Len() {
				methods = append(methods, sd.Methods().Get(j))
			}
		}
		return true
	})
	if len(methods) != 8 {
		t.Errorf("cas.v1 has %d methods, want 8", len(methods))
	}

	for _, md := range methods {
		name := "/" + string(md.Parent().FullName()) + "/" + string(md.Name())
		t.Run(string(md.Name()), func(t *testing.T) {
			// The interceptor covers unary calls only: a streaming method needs a stream interceptor first.
			if md.IsStreamingClient() || md.IsStreamingServer() {
				t.Fatalf("%s is a streaming method: add stream authentication", name)
			}
			call := func(callCtx context.Context) error {
				in := newMessage(t, md.Input())
				out := newMessage(t, md.Output())
				return conn.Invoke(callCtx, name, in, out)
			}
			assertCode(t, "without a token", call(ctx), codes.Unauthenticated)
			assertCode(t, "with a valid token", call(bearer(tok.Value)), codes.Unimplemented)
		})
	}
}

func newMessage(t *testing.T, md protoreflect.MessageDescriptor) protoreflect.ProtoMessage {
	t.Helper()
	mt, err := protoregistry.GlobalTypes.FindMessageByName(md.FullName())
	if err != nil {
		t.Fatal(err)
	}
	return mt.New().Interface()
}

// C1-T412 — Req: UC-105
func TestT412_SeveralValidTokens(t *testing.T) {
	e := setup(t)
	e.tenant(t, "tenant-a")
	first := e.issue(t, "tenant-a")
	second := e.issue(t, "tenant-a")
	conn := e.realServer(t)

	assertCode(t, "first", listSources(conn, bearer(first.Value)), codes.Unimplemented)
	assertCode(t, "second", listSources(conn, bearer(second.Value)), codes.Unimplemented)

	if _, err := e.reg.RevokeToken(ctx, first.KeyID); err != nil {
		t.Fatal(err)
	}
	assertCode(t, "first after revoke", listSources(conn, bearer(first.Value)), codes.Unauthenticated)
	assertCode(t, "second after revoke", listSources(conn, bearer(second.Value)), codes.Unimplemented)
}
