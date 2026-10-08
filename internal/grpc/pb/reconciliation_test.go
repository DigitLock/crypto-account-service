package pb_test

import (
	"slices"
	"testing"

	"google.golang.org/protobuf/reflect/protoreflect"

	casv1 "github.com/DigitLock/crypto-account-service/internal/grpc/pb/cas/v1"
)

// S3-T101 — Req: SRS — Core §2.1.1 (GetReconciliationReport, Reconciliation run); S3 D-10, S3 D-21. buf lint and
// buf breaking against the image of v0.3.0 run in make proto-check.
func TestT101_GetReconciliationReportContract(t *testing.T) {
	fd := casv1.File_cas_v1_card_service_proto

	t.Run("method", func(t *testing.T) {
		md := fd.Services().ByName("CardService").Methods().ByName("GetReconciliationReport")
		if md == nil {
			t.Fatal("CardService.GetReconciliationReport is missing")
		}
		if md.IsStreamingClient() || md.IsStreamingServer() {
			t.Error("GetReconciliationReport is a streaming method")
		}
		if md.Input().FullName() != "cas.v1.GetReconciliationReportRequest" ||
			md.Output().FullName() != "cas.v1.GetReconciliationReportResponse" {
			t.Errorf("messages %s and %s", md.Input().FullName(), md.Output().FullName())
		}
	})

	type f struct {
		name    string
		number  protoreflect.FieldNumber
		kind    protoreflect.Kind
		message string // full name of a message or an enum
		list    bool
	}
	str := protoreflect.StringKind
	i64 := protoreflect.Int64Kind
	msg := protoreflect.MessageKind
	ts := "google.protobuf.Timestamp"

	t.Run("fields", func(t *testing.T) {
		for name, want := range map[string][]f{
			"GetReconciliationReportRequest":  {{"source", 1, str, "", false}, {"run_id", 2, str, "", false}},
			"GetReconciliationReportResponse": {{"run", 1, msg, "cas.v1.ReconciliationRun", false}},
			"ReconciliationRun": {
				{"run_id", 1, str, "", false},
				{"source", 2, str, "", false},
				{"period_from", 3, msg, ts, false},
				{"period_to", 4, msg, ts, false},
				{"to_block", 5, str, "", false},
				{"totals", 6, msg, "cas.v1.ReconciliationTotals", false},
				{"mismatches", 7, msg, "cas.v1.ReconciliationMismatch", true},
				{"created_at", 8, msg, ts, false},
			},
			"ReconciliationTotals": {
				{"authorizations_checked", 1, i64, "", false},
				{"debits_count", 2, i64, "", false},
				{"debits_amount", 3, str, "", false},
				{"returns_checked", 4, i64, "", false},
				{"refunds_count", 5, i64, "", false},
				{"refunds_amount", 6, str, "", false},
			},
			"ReconciliationMismatch": {
				{"type", 1, protoreflect.EnumKind, "cas.v1.ReconciliationMismatchType", false},
				{"auth_id", 2, str, "", false},
				{"return_id", 3, str, "", false},
				{"chain_auth_id", 4, str, "", false},
				{"chain_refund_id", 5, str, "", false},
				{"tx_hash", 6, str, "", false},
				{"expected_amount", 7, str, "", false},
				{"actual_amount", 8, str, "", false},
				{"asset", 9, str, "", false},
				{"checkpoint_balance", 10, str, "", false},
				{"ledger_total", 11, str, "", false},
			},
		} {
			md := message(t, name)
			if md.ParentFile().Path() != fd.Path() {
				t.Errorf("%s is in %s, want %s", name, md.ParentFile().Path(), fd.Path())
			}
			var names []string
			for _, w := range want {
				names = append(names, w.name)
			}
			if got := fieldNames(md); !slices.Equal(got, names) {
				t.Errorf("%s fields = %v, want %v", name, got, names)
				continue
			}
			for _, w := range want {
				fld := md.Fields().ByName(protoreflect.Name(w.name))
				var full string
				switch fld.Kind() {
				case protoreflect.EnumKind:
					full = string(fld.Enum().FullName())
				case protoreflect.MessageKind:
					full = string(fld.Message().FullName())
				}
				if fld.Number() != w.number || fld.Kind() != w.kind || full != w.message || fld.IsList() != w.list ||
					fld.HasPresence() && fld.Kind() != protoreflect.MessageKind {
					t.Errorf("%s.%s = #%d %v %s list=%v presence=%v, want #%d %v %s list=%v", name, w.name,
						fld.Number(), fld.Kind(), full, fld.IsList(), fld.HasPresence(), w.number, w.kind, w.message, w.list)
				}
			}
		}
	})

	t.Run("mismatch types", func(t *testing.T) {
		ed := fd.Enums().ByName("ReconciliationMismatchType")
		if ed == nil {
			t.Fatal("enum ReconciliationMismatchType is missing")
		}
		want := []string{
			"UNSPECIFIED", "MISSING_DEBIT", "AMOUNT_MISMATCH", "UNKNOWN_DEBIT", "UNEXPECTED_DEBIT", "MISSING_REFUND",
			"UNKNOWN_REFUND", "TREASURY_MISMATCH",
		}
		if ed.Values().Len() != len(want) {
			t.Errorf("ReconciliationMismatchType has %d values, want %d", ed.Values().Len(), len(want))
		}
		for n, v := range want {
			name := "RECONCILIATION_MISMATCH_TYPE_" + v
			vd := ed.Values().ByNumber(protoreflect.EnumNumber(n))
			if vd == nil || string(vd.Name()) != name {
				t.Errorf("value %d is %v, want %s", n, vd, name)
			}
		}
	})
}
