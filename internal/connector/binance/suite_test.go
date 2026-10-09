package binance

import (
	"bytes"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/DigitLock/crypto-account-service/internal/connector"
	"github.com/DigitLock/crypto-account-service/internal/connector/connectortest"
	"github.com/DigitLock/crypto-account-service/internal/httpfixture"
)

// suiteFiles are the committed fixtures of each case of the shared suite.
var suiteFiles = map[string]string{
	string(connectortest.SnapshotComplete):    "snapshot_full.json",
	string(connectortest.SnapshotSourceFails): "snapshot_flexible_fails.json",
	string(connectortest.SnapshotRateLimited): "snapshot_funding_fails.json",
	string(connectortest.KeyReadOnly):         "key_read_only.json",
	string(connectortest.KeyNotReadOnly):      "key_not_read_only.json",
	string(connectortest.KeyRejected):         "key_rejected.json",
}

// suiteConnector serves the fixture of a case and returns a connector on a fixed clock, so the same answers give the
// same time, and the source pointing at the fake server with the alias of LDO.
func suiteConnector(t *testing.T, c string) (*Connector, connector.Source, *httpfixture.Server, int) {
	t.Helper()
	f := fixture(t, suiteFiles[c])
	srv := httpfixture.Serve(t, f, weights)
	conn := New(nil, nil)
	conn.now = func() time.Time { return t0 }
	src := source(`{"base_url": "` + srv.URL() + `"}`)
	src.Aliases = []connector.Alias{ldoAlias}
	return conn, src, srv, len(f.Calls)
}

// X1-T501 — Req: FR-216, ADR-2; X1 D-12. The Binance connector passes the snapshot and key-check parts of the shared
// connector test suite on the committed fixtures, with no network access. It has no ledger stream in X1.
func TestT501_SharedConnectorSuite(t *testing.T) {
	connectortest.RunSuite(t, connectortest.Suite{
		Snapshot: func(t *testing.T, c connectortest.SnapshotCase) connectortest.SnapshotSetup {
			conn, src, srv, answers := suiteConnector(t, string(c))
			return connectortest.SnapshotSetup{
				Connector: conn,
				Conn:      connector.Connection{ID: "0b0b0b0b-0000-4000-8000-000000000501", Source: src, Account: "100000001", Key: testKey(t)},
				Served:    func() (int, int) { return srv.Served(), answers },
			}
		},
		KeyCheck: func(t *testing.T, c connectortest.KeyCase) connectortest.KeySetup {
			conn, src, srv, answers := suiteConnector(t, string(c))
			return connectortest.KeySetup{
				Connector: conn, Source: src, Credentials: connector.Credentials{ExchangeKey: testKey(t)},
				Permission: "enableWithdrawals",
				Served:     func() (int, int) { return srv.Served(), answers },
			}
		},
	})
}

// hexSecret is the form of a signature or a key of 64 hexadecimal characters.
var hexSecret = regexp.MustCompile(`[0-9a-fA-F]{64}`)

// uids returns the values of every member named uid of a JSON document, at any depth.
func uids(v any) []string {
	var out []string
	switch x := v.(type) {
	case map[string]any:
		for k, val := range x {
			if k == "uid" {
				out = append(out, string(mustJSON(val)))
			}
			out = append(out, uids(val)...)
		}
	case []any:
		for _, val := range x {
			out = append(out, uids(val)...)
		}
	}
	return out
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

// X1-T502 — Req: FR-217. Every file of testdata/fixtures/binance/: a valid fixture; no X-MBX-APIKEY, no signature
// parameter or member, no hexadecimal value of 64 characters, no timestamp, no URL, no host; every uid is the
// fictitious 100000001; the key markers of the tests appear nowhere. The word Signature of Binance's own message of
// -1022 is not a signature.
func TestT502_FixtureScan(t *testing.T) {
	files, err := filepath.Glob(filepath.Join(fixturesDir, "*"))
	if err != nil || len(files) < 15 {
		t.Fatalf("%d fixture files: %v", len(files), err)
	}
	uidCount := 0
	for _, path := range files {
		name := filepath.Base(path)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := httpfixture.Read(path); err != nil {
			t.Errorf("%s: %v", name, err)
		}
		lower := strings.ToLower(string(data))
		for _, s := range []string{"x-mbx-apikey", "signature=", `"signature"`, "timestamp", "://", "127.0.0.1", "localhost",
			"binance.com", "binance.vision", "markerkey", "markersecret", "fictitious-"} {
			if strings.Contains(lower, s) {
				t.Errorf("%s contains %q", name, s)
			}
		}
		if m := hexSecret.Find(data); m != nil {
			t.Errorf("%s contains a hexadecimal value of 64 characters", name)
		}
		var doc any
		dec := json.NewDecoder(bytes.NewReader(data))
		dec.UseNumber()
		if err := dec.Decode(&doc); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for _, u := range uids(doc) {
			uidCount++
			if u != strconv.Itoa(httpfixture.FictitiousUID) {
				t.Errorf("%s: a uid that is not the fictitious one (%d characters)", name, len(u))
			}
		}
	}
	if uidCount == 0 {
		t.Error("no uid found: the scan does not see the account answers")
	}
}

// x1Endpoints are the endpoints of SRS — Binance §2.1.2 marked X1: all read endpoints. get-funding-asset is a POST of
// type USER_DATA that reads (ADR-3).
var x1Endpoints = []string{
	"GET /api/v3/time",
	"GET /api/v3/account",
	"GET /sapi/v1/account/apiRestrictions",
	"POST /sapi/v1/asset/get-funding-asset",
	"GET /sapi/v1/simple-earn/flexible/position",
	"GET /sapi/v1/simple-earn/locked/position",
}

// X1-T505 — Req: ADR-3; SRS — Binance §2.1.2. The endpoint rows are exactly the read endpoints of X1; every row is
// declared in the var block of endpoints and named in it; one place of the package builds a request and one sends
// it, from an endpoint row. No other path can be sent by the session.
func TestT505_ReadOnlyByConstruction(t *testing.T) {
	var got []string
	for _, ep := range endpoints {
		got = append(got, ep.method+" "+ep.path)
		if ep.method != http.MethodGet && ep != endpointFunding {
			t.Errorf("%s %s: only GET, and the POST of get-funding-asset", ep.method, ep.path)
		}
	}
	if !slices.Equal(got, x1Endpoints) {
		t.Errorf("endpoint rows =\n %v\nwant\n %v", got, x1Endpoints)
	}

	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi os.FileInfo) bool { return !strings.HasSuffix(fi.Name(), "_test.go") }, 0)
	if err != nil {
		t.Fatal(err)
	}
	named := map[string]bool{"endpointTime": true, "endpointAccount": true, "endpointRestrictions": true,
		"endpointFunding": true, "endpointFlexible": true, "endpointLocked": true}
	var builds, sends int
	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			// Endpoint rows are built only in the values of the named package variables.
			allowed := map[ast.Node]bool{}
			for _, decl := range file.Decls {
				// The body of sapiEndpoint builds the /sapi rows; its calls are checked below.
				if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == "sapiEndpoint" {
					allowed[fn.Body] = true
					continue
				}
				gen, ok := decl.(*ast.GenDecl)
				if !ok || gen.Tok != token.VAR {
					continue
				}
				for _, spec := range gen.Specs {
					vs := spec.(*ast.ValueSpec)
					for i, name := range vs.Names {
						if named[name.Name] && i < len(vs.Values) {
							allowed[vs.Values[i]] = true
						}
					}
				}
			}
			var stack []ast.Node
			ast.Inspect(file, func(n ast.Node) bool {
				if n == nil {
					stack = stack[:len(stack)-1]
					return true
				}
				stack = append(stack, n)
				inAllowed := slices.ContainsFunc(stack, func(s ast.Node) bool { return allowed[s] })
				switch x := n.(type) {
				case *ast.CompositeLit:
					if id, ok := x.Type.(*ast.Ident); ok && id.Name == "endpoint" && !inAllowed {
						t.Errorf("%s: an endpoint row built outside the named rows", fset.Position(x.Pos()))
					}
				case *ast.CallExpr:
					switch fn := x.Fun.(type) {
					case *ast.Ident:
						if fn.Name == "sapiEndpoint" && !inAllowed {
							t.Errorf("%s: sapiEndpoint outside the named rows", fset.Position(x.Pos()))
						}
					case *ast.SelectorExpr:
						switch fn.Sel.Name {
						case "NewRequest", "NewRequestWithContext":
							builds++
						case "Do", "Get", "Post", "PostForm", "Head":
							if id, ok := fn.X.(*ast.SelectorExpr); ok && id.Sel.Name == "client" {
								sends++
							}
						}
					}
				}
				return true
			})
		}
	}
	if builds != 1 || sends != 1 {
		t.Errorf("requests built in %d places and sent in %d, want one each: session.send", builds, sends)
	}
}
