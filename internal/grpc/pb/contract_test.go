// Package pb_test checks the generated contract against SRS — Core §2.1.1 – §2.1.4 (docs/test-plan-c1.md Phase 2).
// T201, T204 and T205 are covered by make proto-check and CI.
package pb_test

import (
	"slices"
	"strings"
	"testing"

	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"

	casv1 "github.com/DigitLock/crypto-account-service/internal/grpc/pb/cas/v1"
)

const pkg protoreflect.FullName = "cas.v1"

// files returns the descriptors of the files of package cas.v1.
func files(t *testing.T) []protoreflect.FileDescriptor {
	t.Helper()
	// Referencing the generated package registers its files.
	_ = casv1.File_cas_v1_connection_service_proto
	_ = casv1.File_cas_v1_account_data_service_proto

	var fds []protoreflect.FileDescriptor
	protoregistry.GlobalFiles.RangeFilesByPackage(pkg, func(fd protoreflect.FileDescriptor) bool {
		fds = append(fds, fd)
		return true
	})
	if len(fds) == 0 {
		t.Fatal("no file of package cas.v1 is registered")
	}
	return fds
}

func message(t *testing.T, name string) protoreflect.MessageDescriptor {
	t.Helper()
	d, err := protoregistry.GlobalFiles.FindDescriptorByName(pkg.Append(protoreflect.Name(name)))
	if err != nil {
		t.Fatalf("message %s: %v", name, err)
	}
	md, ok := d.(protoreflect.MessageDescriptor)
	if !ok {
		t.Fatalf("%s is not a message", name)
	}
	return md
}

func field(t *testing.T, msg, name string) protoreflect.FieldDescriptor {
	t.Helper()
	fd := message(t, msg).Fields().ByName(protoreflect.Name(name))
	if fd == nil {
		t.Fatalf("%s has no field %s", msg, name)
	}
	return fd
}

func fieldNames(md protoreflect.MessageDescriptor) []string {
	var names []string
	for i := range md.Fields().Len() {
		names = append(names, string(md.Fields().Get(i).Name()))
	}
	return names
}

// allMessages walks every message of the package, nested ones included.
func allMessages(fds []protoreflect.FileDescriptor, visit func(protoreflect.MessageDescriptor)) {
	var walk func(protoreflect.MessageDescriptors)
	walk = func(mds protoreflect.MessageDescriptors) {
		for i := range mds.Len() {
			visit(mds.Get(i))
			walk(mds.Get(i).Messages())
		}
	}
	for _, fd := range fds {
		walk(fd.Messages())
	}
}

// C1-T202 — Req: §2.1.1
func TestT202_MethodCatalogue(t *testing.T) {
	want := map[string][]string{
		"ConnectionService": {
			"ListSources", "CreateConnection", "ListConnections", "GetConnection", "DeleteConnection", "TriggerSync",
		},
		"AccountDataService": {"GetBalances", "ListLedgerEntries"},
	}

	got := map[string][]string{}
	for _, fd := range files(t) {
		for i := range fd.Services().Len() {
			sd := fd.Services().Get(i)
			var methods []string
			for j := range sd.Methods().Len() {
				methods = append(methods, string(sd.Methods().Get(j).Name()))
			}
			got[string(sd.Name())] = methods
		}
	}

	if len(got) != len(want) {
		t.Errorf("services = %v, want exactly %v", keys(got), keys(want))
	}
	for svc, methods := range want {
		if !slices.Equal(got[svc], methods) {
			t.Errorf("%s methods = %v, want %v", svc, got[svc], methods)
		}
	}
}

func keys(m map[string][]string) []string {
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	slices.Sort(ks)
	return ks
}

// C1-T203 — Req: §2.1.1 – §2.1.4, handoff §4
func TestT203_MessagesAgainstSRS(t *testing.T) {
	fds := files(t)

	t.Run("no float or double field", func(t *testing.T) {
		allMessages(fds, func(md protoreflect.MessageDescriptor) {
			for i := range md.Fields().Len() {
				fd := md.Fields().Get(i)
				if k := fd.Kind(); k == protoreflect.FloatKind || k == protoreflect.DoubleKind {
					t.Errorf("%s is %v", fd.FullName(), k)
				}
			}
		})
	})

	t.Run("amounts are strings", func(t *testing.T) {
		for _, f := range [][2]string{{"Balance", "free"}, {"Balance", "locked"}, {"LedgerEntry", "amount"}} {
			fd := field(t, f[0], f[1])
			if fd.Kind() != protoreflect.StringKind || fd.IsList() {
				t.Errorf("%s.%s is %v, want string", f[0], f[1], fd.Kind())
			}
		}
	})

	t.Run("seq is int64", func(t *testing.T) {
		for _, f := range [][2]string{
			{"LedgerEntry", "seq"},
			{"ListLedgerEntriesRequest", "after_seq"},
			{"ListLedgerEntriesResponse", "last_seq"},
		} {
			fd := field(t, f[0], f[1])
			if fd.Kind() != protoreflect.Int64Kind || fd.IsList() {
				t.Errorf("%s.%s is %v, want int64", f[0], f[1], fd.Kind())
			}
		}
	})

	t.Run("oneof selectors", func(t *testing.T) {
		for _, o := range []struct {
			msg, oneof string
			fields     []string
		}{
			{"CreateConnectionRequest", "credential", []string{"exchange_key", "wallet"}},
			{"GetBalancesRequest", "selector", []string{"owner_ref", "connection_id"}},
		} {
			od := message(t, o.msg).Oneofs().ByName(protoreflect.Name(o.oneof))
			if od == nil || od.IsSynthetic() {
				t.Errorf("%s has no oneof %s", o.msg, o.oneof)
				continue
			}
			var got []string
			for i := range od.Fields().Len() {
				got = append(got, string(od.Fields().Get(i).Name()))
			}
			if !slices.Equal(got, o.fields) {
				t.Errorf("%s.%s = %v, want %v", o.msg, o.oneof, got, o.fields)
			}
		}
	})

	t.Run("field names", func(t *testing.T) {
		for msg, want := range map[string][]string{
			// §2.1.2 response.
			"Connection": {
				"connection_id", "source", "kind", "owner_ref", "label", "status", "key_fingerprint", "permissions",
				"created_at",
			},
			// §2.1.1 method table, GetConnection.
			"StreamHealth": {
				"stream", "mode", "next_run_at", "last_success_at", "last_error", "consecutive_failures",
			},
			// §2.1.3 response.
			"ConnectionSnapshot": {"connection_id", "source", "as_of", "stale"},
			"Balance":            {"connection_id", "account_type", "asset", "native_asset", "free", "locked"},
			// §2.1.4 response.
			"LedgerEntry": {
				"seq", "connection_id", "type", "leg", "direction", "asset", "native_asset", "amount", "group_id",
				"external_id", "occurred_at",
			},
		} {
			if got := fieldNames(message(t, msg)); !slices.Equal(got, want) {
				t.Errorf("%s fields = %v, want %v", msg, got, want)
			}
		}
	})

	t.Run("enums", func(t *testing.T) {
		want := map[string][]string{
			"SourceKind":       {"EXCHANGE", "EVM"},                                           // §2.4 sources.kind
			"ConnectionKind":   {"EXCHANGE", "EVM_WALLET"},                                    // §2.1.2
			"ConnectionStatus": {"ACTIVE", "DEGRADED", "CREDENTIALS_INVALID"},                 // §2.3.1
			"StreamMode":       {"BACKFILL", "INCREMENTAL"},                                   // §2.3.2
			"AccountType":      {"SPOT", "FUNDING", "EARN_FLEXIBLE", "EARN_LOCKED", "WALLET"}, // §2.1.3
			"LedgerEntryType": {
				"DEPOSIT", "WITHDRAWAL", "TRADE", "FEE", "CONVERT", "REWARD", "CARD_DEBIT", "CARD_REFUND",
			}, // §2.1.4
			"LedgerLeg":       {"SINGLE", "BASE", "QUOTE", "FEE"}, // §2.1.4
			"LedgerDirection": {"IN", "OUT"},                      // §2.1.4
		}

		seen := map[string]bool{}
		for _, fd := range fds {
			for i := range fd.Enums().Len() {
				ed := fd.Enums().Get(i)
				name := string(ed.Name())
				seen[name] = true
				values, ok := want[name]
				if !ok {
					t.Errorf("enum %s is not in the SRS list of this test", name)
					continue
				}
				prefix := screamingSnake(name) + "_"
				wantValues := []string{prefix + "UNSPECIFIED"}
				for _, v := range values {
					wantValues = append(wantValues, prefix+v)
				}
				var got []string
				for j := range ed.Values().Len() {
					got = append(got, string(ed.Values().Get(j).Name()))
				}
				if !slices.Equal(got, wantValues) {
					t.Errorf("%s = %v, want %v", name, got, wantValues)
				}
				if ed.Values().ByNumber(0) == nil || string(ed.Values().ByNumber(0).Name()) != prefix+"UNSPECIFIED" {
					t.Errorf("%s: value 0 is not %sUNSPECIFIED", name, prefix)
				}
			}
		}
		for name := range want {
			if !seen[name] {
				t.Errorf("enum %s is missing", name)
			}
		}
	})
}

// screamingSnake turns LedgerEntryType into LEDGER_ENTRY_TYPE.
func screamingSnake(s string) string {
	var b strings.Builder
	for i, r := range s {
		if i > 0 && r >= 'A' && r <= 'Z' {
			b.WriteByte('_')
		}
		b.WriteRune(r)
	}
	return strings.ToUpper(b.String())
}
