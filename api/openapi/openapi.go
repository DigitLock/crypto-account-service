// Package openapi embeds the contract of the processor API of card-auth (SRS — Card Spend §2.1).
// The source is card-auth.yaml; frozen/card-auth.yaml is its frozen copy (make openapi-check).
// No code is generated from it: handlers are written by hand and their responses are validated against it in tests.
package openapi

import (
	"context"
	_ "embed"
	"fmt"

	"github.com/getkin/kin-openapi/openapi3"
)

// Spec holds card-auth.yaml.
//
//go:embed card-auth.yaml
var Spec []byte

// Load parses Spec and validates it as OpenAPI 3.0, the examples included.
func Load() (*openapi3.T, error) {
	doc, err := openapi3.NewLoader().LoadFromData(Spec)
	if err != nil {
		return nil, fmt.Errorf("openapi: load card-auth.yaml: %w", err)
	}
	if err := doc.Validate(context.Background(), openapi3.EnableExamplesValidation()); err != nil {
		return nil, fmt.Errorf("openapi: validate card-auth.yaml: %w", err)
	}
	return doc, nil
}
