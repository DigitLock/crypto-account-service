package vault

import (
	"fmt"
	"log/slog"
)

// Secret holds a value that must never be printed: the master key, a connection string, an exchange
// key or secret. Every fmt verb, String, GoString, LogValue and text and JSON marshalling yield
// "[redacted]"; Value returns the value itself.
type Secret[T []byte | string] struct {
	v T
}

// NewSecret wraps v.
func NewSecret[T []byte | string](v T) Secret[T] { return Secret[T]{v: v} }

const redacted = "[redacted]"

// Value returns the secret value.
func (s Secret[T]) Value() T { return s.v }

func (Secret[T]) String() string               { return redacted }
func (Secret[T]) GoString() string             { return redacted }
func (Secret[T]) Format(f fmt.State, _ rune)   { _, _ = f.Write([]byte(redacted)) }
func (Secret[T]) LogValue() slog.Value         { return slog.StringValue(redacted) }
func (Secret[T]) MarshalText() ([]byte, error) { return []byte(redacted), nil }
