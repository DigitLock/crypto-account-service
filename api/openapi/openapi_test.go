// Phase 2 of docs/test-plan-s2.md: the OpenAPI contract of the processor API against SRS — Card Spend §2.1.
package openapi_test

import (
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"

	"github.com/DigitLock/crypto-account-service/api/openapi"
)

func load(t *testing.T) *openapi3.T {
	t.Helper()
	doc, err := openapi.Load()
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

// S2-T201 — Req: ADR-7, §2.1
func TestT201_OpenAPIValid(t *testing.T) {
	doc := load(t)
	if doc.OpenAPI != "3.0.3" {
		t.Errorf("openapi = %q, want 3.0.3", doc.OpenAPI)
	}
	if doc.Info == nil || doc.Info.Version != "1.0.0" || doc.Info.Title == "" ||
		!strings.Contains(doc.Info.Description, "docs/srs/card-spend.md") {
		t.Errorf("info = %+v, want a title, version 1.0.0 and a pointer to docs/srs/card-spend.md", doc.Info)
	}
}

// operation names one operation of §2.1 with the codes it answers and the error codes of each error status.
type operation struct {
	method, path string
	errors       map[string][]string // status → error codes of §2.1.1
	success      string              // schema of the 200 body
	request      string              // schema of the request body; empty without a body
}

var operations = []operation{
	{
		method: "POST", path: "/v1/authorizations", success: "AuthorizeResponse", request: "AuthorizeRequest",
		errors: map[string][]string{"401": {"UNAUTHENTICATED"}, "409": {"AUTH_ID_CONFLICT"}, "422": {"INVALID_REQUEST"}},
	},
	{
		method: "POST", path: "/v1/authorizations/{auth_id}/returns", success: "ReturnResponse", request: "ReturnRequest",
		errors: map[string][]string{
			"401": {"UNAUTHENTICATED"},
			"409": {"AUTHORIZATION_IN_PROGRESS", "RETURN_ID_CONFLICT"},
			"422": {"INVALID_REQUEST", "RETURN_EXCEEDS_DEBIT"},
		},
	},
	{
		method: "GET", path: "/v1/authorizations/{auth_id}", success: "Authorization",
		errors: map[string][]string{"401": {"UNAUTHENTICATED"}, "404": {"NOT_FOUND"}},
	},
}

// fields of §2.1.2 – §2.1.4: every property of the schema, and the required ones.
var fields = map[string]struct{ required, optional []string }{
	"AuthorizeRequest":  {[]string{"auth_id", "card_ref", "amount", "currency"}, []string{"merchant"}},
	"AuthorizeResponse": {[]string{"auth_id", "decision", "status"}, []string{"decline_reason", "token", "token_amount", "quote", "tx_hash"}},
	"ReturnRequest":     {[]string{"return_id", "type"}, []string{"amount"}},
	"ReturnResponse":    {[]string{"return_id", "auth_id", "status", "token_amount"}, nil},
	"Authorization": {
		[]string{"auth_id", "status", "debited_amount", "returned_amount", "returns", "history"},
		[]string{"decline_reason", "amount", "currency", "token", "token_amount", "quote", "tx_hash"},
	},
	"Return":       {[]string{"return_id", "type", "status", "token_amount"}, []string{"tx_hash"}},
	"StatusChange": {[]string{"status", "at"}, nil},
	"Quote":        {[]string{"buffer_bps"}, []string{"rate"}},
	"Merchant":     {nil, []string{"name", "mcc", "country"}},
	"Error":        {[]string{"error"}, nil},
}

// enums of §2.1.1 – §2.1.4, §2.3.1 and §2.3.2.
var enums = map[string][]string{
	"Decision": {"APPROVED", "DECLINED"},
	"AuthorizationStatus": {
		"RECEIVED", "DEBIT_SUBMITTED", "APPROVED", "DEBIT_CONFIRMED", "DECLINED", "TIMED_OUT", "LATE_DEBIT",
		"LATE_DEBIT_REFUNDED", "DEBIT_LOST",
	},
	"DeclineReason": {
		"CARD_NOT_FOUND", "CARD_FROZEN", "PROGRAM_PAUSED", "CURRENCY_NOT_SUPPORTED", "RATE_UNAVAILABLE",
		"LIMIT_EXCEEDED", "INSUFFICIENT_FUNDS", "INSUFFICIENT_ALLOWANCE", "CHAIN_UNAVAILABLE", "DEBIT_REVERTED",
		"TIMEOUT", "REVERSED_BEFORE_AUTH", "INTERNAL_ERROR",
	},
	"ReturnRequestType": {"REVERSAL", "REFUND"},
	"ReturnType":        {"REVERSAL", "REFUND", "LATE_DEBIT"},
	"ReturnStatus":      {"ACCEPTED", "SUBMITTED", "INCLUDED", "CONFIRMED", "RETRYING", "NOTHING_TO_RETURN"},
	"ErrorCode": {
		"UNAUTHENTICATED", "INVALID_REQUEST", "AUTH_ID_CONFLICT", "RETURN_ID_CONFLICT", "AUTHORIZATION_IN_PROGRESS",
		"RETURN_EXCEEDS_DEBIT", "NOT_FOUND",
	},
}

// resolved follows a property defined as allOf of one schema with its own description.
func resolved(s *openapi3.Schema) *openapi3.Schema {
	if (s.Type == nil || len(*s.Type) == 0) && len(s.AllOf) == 1 {
		return resolved(s.AllOf[0].Value)
	}
	return s
}

func schema(t *testing.T, doc *openapi3.T, name string) *openapi3.Schema {
	t.Helper()
	ref, ok := doc.Components.Schemas[name]
	if !ok || ref.Value == nil {
		t.Fatalf("schema %s is missing", name)
	}
	return ref.Value
}

func jsonBody(t *testing.T, content openapi3.Content, what string) *openapi3.MediaType {
	t.Helper()
	mt := content.Get("application/json")
	if mt == nil || mt.Schema == nil {
		t.Fatalf("%s has no application/json body", what)
	}
	return mt
}

// errorCodes returns the error codes of the examples of an error response.
func errorCodes(mt *openapi3.MediaType) []string {
	var values []any
	if mt.Example != nil {
		values = append(values, mt.Example)
	}
	for _, ex := range mt.Examples {
		values = append(values, ex.Value.Value)
	}
	var codes []string
	for _, v := range values {
		if body, ok := v.(map[string]any); ok {
			if e, ok := body["error"].(map[string]any); ok {
				codes = append(codes, e["code"].(string))
			}
		}
	}
	slices.Sort(codes)
	return codes
}

// S2-T202 — Req: §2.1.1 – §2.1.4
func TestT202_OpenAPIAgainstSRS(t *testing.T) {
	doc := load(t)

	t.Run("operations, codes and security", func(t *testing.T) {
		var got []string
		for path, item := range doc.Paths.Map() {
			for method := range item.Operations() {
				got = append(got, method+" "+path)
			}
		}
		var want []string
		for _, op := range operations {
			want = append(want, op.method+" "+op.path)
		}
		slices.Sort(got)
		slices.Sort(want)
		if !slices.Equal(got, want) {
			t.Fatalf("operations = %v, want exactly %v", got, want)
		}

		scheme := doc.Components.SecuritySchemes["basicAuth"]
		if scheme == nil || scheme.Value.Type != "http" || scheme.Value.Scheme != "basic" {
			t.Fatalf("securitySchemes.basicAuth is not HTTP basic")
		}

		for _, o := range operations {
			op := doc.Paths.Find(o.path).GetOperation(o.method)
			name := o.method + " " + o.path

			if op.Security == nil || len(*op.Security) != 1 || (*op.Security)[0]["basicAuth"] == nil {
				t.Errorf("%s: security is not basicAuth", name)
			}

			wantCodes := slices.Sorted(maps.Keys(o.errors))
			wantCodes = append(wantCodes, "200")
			slices.Sort(wantCodes)
			if got := slices.Sorted(maps.Keys(op.Responses.Map())); !slices.Equal(got, wantCodes) {
				t.Errorf("%s: response codes = %v, want %v", name, got, wantCodes)
			}

			if s := jsonBody(t, op.Responses.Status(200).Value.Content, name+" 200").Schema; s.Ref != "#/components/schemas/"+o.success {
				t.Errorf("%s: 200 body is %q, want %s", name, s.Ref, o.success)
			}
			for status, codes := range o.errors {
				resp := op.Responses.Value(status).Value
				mt := jsonBody(t, resp.Content, name+" "+status)
				if mt.Schema.Ref != "#/components/schemas/Error" {
					t.Errorf("%s %s: body is %q, want the Error schema", name, status, mt.Schema.Ref)
				}
				want := slices.Clone(codes)
				slices.Sort(want)
				if got := errorCodes(mt); !slices.Equal(got, want) {
					t.Errorf("%s %s: example error codes = %v, want %v", name, status, got, want)
				}
				for _, c := range codes {
					if resp.Description == nil || !strings.Contains(*resp.Description, c) {
						t.Errorf("%s %s: description does not name %s", name, status, c)
					}
				}
			}

			if o.request != "" {
				if op.RequestBody == nil || !op.RequestBody.Value.Required {
					t.Fatalf("%s: request body is not required", name)
				}
				if s := jsonBody(t, op.RequestBody.Value.Content, name+" request").Schema; s.Ref != "#/components/schemas/"+o.request {
					t.Errorf("%s: request body is %q, want %s", name, s.Ref, o.request)
				}
			}
			if strings.Contains(o.path, "{auth_id}") {
				p := op.Parameters.GetByInAndName("path", "auth_id")
				if p == nil || !p.Required {
					t.Errorf("%s: path parameter auth_id missing or not required", name)
				}
			}
		}
	})

	t.Run("fields and requiredness", func(t *testing.T) {
		for name, f := range fields {
			s := schema(t, doc, name)
			req := slices.Clone(s.Required)
			slices.Sort(req)
			want := slices.Clone(f.required)
			slices.Sort(want)
			if !slices.Equal(req, want) {
				t.Errorf("%s required = %v, want %v", name, req, want)
			}
			props := slices.Sorted(maps.Keys(s.Properties))
			all := append(slices.Clone(f.required), f.optional...)
			slices.Sort(all)
			if !slices.Equal(props, all) {
				t.Errorf("%s properties = %v, want %v", name, props, all)
			}
		}
		errObj := schema(t, doc, "Error").Properties["error"].Value
		if req := slices.Sorted(slices.Values(errObj.Required)); !slices.Equal(req, []string{"code", "message"}) {
			t.Errorf("Error.error required = %v, want [code message]", req)
		}
		if errObj.Properties["code"].Ref != "#/components/schemas/ErrorCode" {
			t.Errorf("Error.error.code is not the ErrorCode enum")
		}
	})

	t.Run("requests allow unknown fields", func(t *testing.T) {
		for _, name := range []string{"AuthorizeRequest", "ReturnRequest", "Merchant"} {
			if ap := schema(t, doc, name).AdditionalProperties; ap.Has != nil && !*ap.Has {
				t.Errorf("%s forbids unknown fields", name)
			}
		}
	})

	t.Run("enums", func(t *testing.T) {
		for name, want := range enums {
			var got []string
			for _, v := range schema(t, doc, name).Enum {
				got = append(got, v.(string))
			}
			if !slices.Equal(got, want) {
				t.Errorf("%s = %v, want %v", name, got, want)
			}
		}
	})

	t.Run("amounts are strings, no number type", func(t *testing.T) {
		amounts := 0
		for name, ref := range doc.Components.Schemas {
			s := ref.Value
			if s.Type.Is("number") {
				t.Errorf("schema %s is a number", name)
			}
			for prop, pref := range s.Properties {
				p := resolved(pref.Value)
				if p.Type.Is("number") {
					t.Errorf("%s.%s is a number", name, prop)
				}
				if strings.Contains(prop, "amount") || prop == "rate" {
					amounts++
					if !p.Type.Is("string") || p.Pattern == "" {
						t.Errorf("%s.%s is not a string with a pattern", name, prop)
					}
				}
			}
		}
		if amounts < 9 {
			t.Errorf("found %d amount fields, want every amount of §2.1.2 – §2.1.4", amounts)
		}
		for _, name := range []string{"TokenAmount"} {
			if p := schema(t, doc, name).Pattern; p != "^(0|[1-9][0-9]*)$" {
				t.Errorf("%s pattern = %q, want a base-unit integer", name, p)
			}
		}
	})

	t.Run("validation of §2.1.1", func(t *testing.T) {
		id := schema(t, doc, "ProcessorId")
		if id.MinLength != 1 || id.MaxLength == nil || *id.MaxLength != 64 {
			t.Errorf("ProcessorId lengths = %d..%v, want 1..64", id.MinLength, id.MaxLength)
		}
		for _, c := range []struct {
			schema string
			ok     []string
			bad    []string
		}{
			{"FiatAmount", []string{"25.40", "25.4", "0.0001", "1000"}, []string{"25.40000", "1e3", "-1", "+1", "01.5", "25.", ".5", ""}},
			{"Currency", []string{"EUR", "USD"}, []string{"eur", "EU", "EURO"}},
			{"TokenAmount", []string{"0", "29866387"}, []string{"-1", "1.5", "01", "1e6", ""}},
			{"ProcessorId", []string{"9f1c2a7e-5b1d-4c58-9a57-0d2f6f1e8a11", "rv-20261002-0001", "a b"}, []string{"", strings.Repeat("a", 65), "tab\there", "ünicode"}},
		} {
			s := schema(t, doc, c.schema)
			for _, v := range c.ok {
				if err := s.VisitJSON(v); err != nil {
					t.Errorf("%s refuses %q: %v", c.schema, v, err)
				}
			}
			for _, v := range c.bad {
				if s.VisitJSON(v) == nil {
					t.Errorf("%s accepts %q", c.schema, v)
				}
			}
		}
		if !strings.Contains(doc.Info.Description, "16 KiB") || !strings.Contains(schema(t, doc, "FiatAmount").Description, "greater than 0") {
			t.Error("the rules OpenAPI cannot express are not in the descriptions")
		}
	})

	t.Run("examples validate", func(t *testing.T) {
		count := 0
		check := func(where string, s *openapi3.Schema, v any) {
			count++
			if err := s.VisitJSON(v); err != nil {
				t.Errorf("example %s: %v", where, err)
			}
		}
		for path, item := range doc.Paths.Map() {
			for method, op := range item.Operations() {
				var bodies []*openapi3.MediaType
				if op.RequestBody != nil {
					bodies = append(bodies, op.RequestBody.Value.Content.Get("application/json"))
				}
				for _, r := range op.Responses.Map() {
					bodies = append(bodies, r.Value.Content.Get("application/json"))
				}
				for _, mt := range bodies {
					if mt.Example != nil {
						check(method+" "+path, mt.Schema.Value, mt.Example)
					}
					for name, ex := range mt.Examples {
						check(method+" "+path+" "+name, mt.Schema.Value, ex.Value.Value)
					}
				}
			}
		}
		// The SRS examples: 2 requests, 2 authorize responses, 1 return response, 2 authorizations.
		if count < 7 {
			t.Errorf("%d examples, want at least the 7 of the SRS", count)
		}
		tomb := doc.Paths.Find("/v1/authorizations/{auth_id}").Get.Responses.Status(200).Value.Content.
			Get("application/json").Examples["tombstone"]
		if tomb == nil {
			t.Fatal("the tombstone example of §2.1.4 is missing")
		}
		v := tomb.Value.Value.(map[string]any)
		if v["status"] != "DECLINED" || v["decline_reason"] != "REVERSED_BEFORE_AUTH" || v["debited_amount"] != "0" ||
			len(v["history"].([]any)) != 1 || v["returns"].([]any)[0].(map[string]any)["status"] != "NOTHING_TO_RETURN" {
			t.Errorf("tombstone example differs from §2.1.4: %v", v)
		}
	})
}
