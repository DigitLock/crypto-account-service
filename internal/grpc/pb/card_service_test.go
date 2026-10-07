package pb_test

import (
	"slices"
	"testing"

	"google.golang.org/protobuf/reflect/protoreflect"

	casv1 "github.com/DigitLock/crypto-account-service/internal/grpc/pb/cas/v1"
)

// S2-T204 — Req: SRS — Core §2.1.1, §2.1.5; SRS — Card Spend §2.1.4. buf lint of the STANDARD rules runs in
// make proto-check.
func TestT204_CardServiceDescriptors(t *testing.T) {
	fd := casv1.File_cas_v1_card_service_proto

	t.Run("six methods of S2", func(t *testing.T) {
		sd := fd.Services().ByName("CardService")
		if sd == nil {
			t.Fatal("CardService is missing")
		}
		var got []string
		for i := range sd.Methods().Len() {
			md := sd.Methods().Get(i)
			got = append(got, string(md.Name()))
			if md.IsStreamingClient() || md.IsStreamingServer() {
				t.Errorf("%s is a streaming method", md.Name())
			}
			if string(md.Input().Name()) != string(md.Name())+"Request" || string(md.Output().Name()) != string(md.Name())+"Response" {
				t.Errorf("%s: messages %s and %s", md.Name(), md.Input().Name(), md.Output().Name())
			}
		}
		// GetReconciliationReport of S3 follows them: S3-T101.
		want := []string{
			"RegisterCard", "UpdateCard", "GetCard", "ListCards", "GetAuthorization", "ListAuthorizations",
			"GetReconciliationReport",
		}
		if !slices.Equal(got, want) {
			t.Errorf("CardService methods = %v, want %v", got, want)
		}
		if fd.Services().Len() != 1 {
			t.Errorf("card_service.proto has %d services, want 1", fd.Services().Len())
		}
	})

	t.Run("field names", func(t *testing.T) {
		for msg, want := range map[string][]string{
			"Card": {
				"card_ref", "owner_ref", "connection_id", "wallet_address", "status", "daily_limit", "created_at", "updated_at",
			},
			"Quote":        {"rate", "buffer_bps"},
			"Return":       {"return_id", "type", "status", "token_amount", "tx_hash", "created_at"},
			"StatusChange": {"status", "reason", "at"},
			"Authorization": {
				"auth_id", "status", "decline_reason", "amount", "currency", "token", "token_amount", "quote",
				"debited_amount", "returned_amount", "tx_hash", "returns", "history", "card_ref", "received_at", "decided_at",
			},
			"RegisterCardRequest":        {"card_ref", "owner_ref", "connection_id", "daily_limit"},
			"RegisterCardResponse":       {"card"},
			"UpdateCardRequest":          {"card_ref", "daily_limit", "status"},
			"UpdateCardResponse":         {"card"},
			"GetCardRequest":             {"card_ref"},
			"GetCardResponse":            {"card"},
			"ListCardsRequest":           {"owner_ref", "page_size", "page_token"},
			"ListCardsResponse":          {"cards", "next_page_token"},
			"GetAuthorizationRequest":    {"auth_id"},
			"GetAuthorizationResponse":   {"authorization"},
			"ListAuthorizationsRequest":  {"card_ref", "owner_ref", "status", "received_from", "received_to", "page_size", "page_token"},
			"ListAuthorizationsResponse": {"authorizations", "next_page_token"},
		} {
			if got := fieldNames(message(t, msg)); !slices.Equal(got, want) {
				t.Errorf("%s fields = %v, want %v", msg, got, want)
			}
		}
	})

	t.Run("optional fields of UpdateCard", func(t *testing.T) {
		for _, name := range []string{"daily_limit", "status"} {
			if !field(t, "UpdateCardRequest", name).HasPresence() {
				t.Errorf("UpdateCardRequest.%s is not optional", name)
			}
		}
		if field(t, "UpdateCardRequest", "card_ref").HasPresence() {
			t.Error("UpdateCardRequest.card_ref is optional")
		}
	})

	t.Run("types", func(t *testing.T) {
		for _, f := range []struct {
			msg, name string
			kind      protoreflect.Kind
			message   string
		}{
			{"Card", "daily_limit", protoreflect.StringKind, ""},
			{"Card", "status", protoreflect.EnumKind, "cas.v1.CardStatus"},
			{"Card", "created_at", protoreflect.MessageKind, "google.protobuf.Timestamp"},
			{"Card", "updated_at", protoreflect.MessageKind, "google.protobuf.Timestamp"},
			{"Authorization", "status", protoreflect.EnumKind, "cas.v1.AuthorizationStatus"},
			{"Authorization", "decline_reason", protoreflect.EnumKind, "cas.v1.DeclineReason"},
			{"Authorization", "amount", protoreflect.StringKind, ""},
			{"Authorization", "token_amount", protoreflect.StringKind, ""},
			{"Authorization", "debited_amount", protoreflect.StringKind, ""},
			{"Authorization", "returned_amount", protoreflect.StringKind, ""},
			{"Authorization", "quote", protoreflect.MessageKind, "cas.v1.Quote"},
			{"Authorization", "returns", protoreflect.MessageKind, "cas.v1.Return"},
			{"Authorization", "history", protoreflect.MessageKind, "cas.v1.StatusChange"},
			{"Authorization", "received_at", protoreflect.MessageKind, "google.protobuf.Timestamp"},
			{"Authorization", "decided_at", protoreflect.MessageKind, "google.protobuf.Timestamp"},
			{"Quote", "rate", protoreflect.StringKind, ""},
			{"Quote", "buffer_bps", protoreflect.Int32Kind, ""},
			{"Return", "type", protoreflect.EnumKind, "cas.v1.ReturnType"},
			{"Return", "status", protoreflect.EnumKind, "cas.v1.ReturnStatus"},
			{"Return", "token_amount", protoreflect.StringKind, ""},
			{"StatusChange", "status", protoreflect.EnumKind, "cas.v1.AuthorizationStatus"},
			{"StatusChange", "at", protoreflect.MessageKind, "google.protobuf.Timestamp"},
			{"ListAuthorizationsRequest", "status", protoreflect.EnumKind, "cas.v1.AuthorizationStatus"},
			{"ListAuthorizationsRequest", "received_from", protoreflect.MessageKind, "google.protobuf.Timestamp"},
			{"ListAuthorizationsRequest", "received_to", protoreflect.MessageKind, "google.protobuf.Timestamp"},
			{"UpdateCardRequest", "daily_limit", protoreflect.StringKind, ""},
			{"UpdateCardRequest", "status", protoreflect.EnumKind, "cas.v1.CardStatus"},
		} {
			fd := field(t, f.msg, f.name)
			if fd.Kind() != f.kind {
				t.Errorf("%s.%s is %v, want %v", f.msg, f.name, fd.Kind(), f.kind)
				continue
			}
			var full string
			switch f.kind {
			case protoreflect.EnumKind:
				full = string(fd.Enum().FullName())
			case protoreflect.MessageKind:
				full = string(fd.Message().FullName())
			}
			if full != f.message {
				t.Errorf("%s.%s is %s, want %s", f.msg, f.name, full, f.message)
			}
			if wantList := f.name == "returns" || f.name == "history"; fd.IsList() != wantList {
				t.Errorf("%s.%s repeated = %v", f.msg, f.name, fd.IsList())
			}
		}
	})

	t.Run("enums", func(t *testing.T) {
		want := map[string][]string{
			"CardStatus": {"ACTIVE", "FROZEN"}, // SRS — Core §2.1.5
			"AuthorizationStatus": { // SRS — Card Spend §2.3.1
				"RECEIVED", "DEBIT_SUBMITTED", "APPROVED", "DEBIT_CONFIRMED", "DECLINED", "TIMED_OUT", "LATE_DEBIT",
				"LATE_DEBIT_REFUNDED", "DEBIT_LOST",
			},
			"DeclineReason": { // SRS — Card Spend §2.1.2
				"CARD_NOT_FOUND", "CARD_FROZEN", "PROGRAM_PAUSED", "CURRENCY_NOT_SUPPORTED", "RATE_UNAVAILABLE",
				"LIMIT_EXCEEDED", "INSUFFICIENT_FUNDS", "INSUFFICIENT_ALLOWANCE", "CHAIN_UNAVAILABLE", "DEBIT_REVERTED",
				"TIMEOUT", "REVERSED_BEFORE_AUTH", "INTERNAL_ERROR",
			},
			"ReturnType":   {"REVERSAL", "REFUND", "LATE_DEBIT"},                                                // SRS — Card Spend §2.4
			"ReturnStatus": {"ACCEPTED", "SUBMITTED", "INCLUDED", "CONFIRMED", "RETRYING", "NOTHING_TO_RETURN"}, // §2.3.2
		}
		// ReconciliationMismatchType of S3 is checked by S3-T101.
		if fd.Enums().Len() != len(want)+1 {
			t.Errorf("card_service.proto has %d enums, want %d", fd.Enums().Len(), len(want)+1)
		}
		for name, values := range want {
			ed := fd.Enums().ByName(protoreflect.Name(name))
			if ed == nil {
				t.Errorf("enum %s is missing", name)
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
			if string(ed.Values().ByNumber(0).Name()) != prefix+"UNSPECIFIED" {
				t.Errorf("%s: value 0 is not %sUNSPECIFIED", name, prefix)
			}
		}
	})

	t.Run("no float or double field in cas.v1", func(t *testing.T) {
		n := 0
		allMessages(files(t), func(md protoreflect.MessageDescriptor) {
			for i := range md.Fields().Len() {
				n++
				if k := md.Fields().Get(i).Kind(); k == protoreflect.FloatKind || k == protoreflect.DoubleKind {
					t.Errorf("%s is %v", md.Fields().Get(i).FullName(), k)
				}
			}
		})
		if n == 0 {
			t.Fatal("no field found")
		}
	})
}
