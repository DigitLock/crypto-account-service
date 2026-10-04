// Package testdb gives integration tests the test database (docs/test-plan-c1.md §1).
package testdb

import (
	"os"
	"testing"
)

// URL returns TEST_DATABASE_URL. When it is empty the test is skipped locally
// and fails in CI, where the variable CI is set.
func URL(t testing.TB) string {
	t.Helper()
	u := os.Getenv("TEST_DATABASE_URL")
	if u == "" {
		if os.Getenv("CI") != "" {
			t.Fatal("TEST_DATABASE_URL is not set; it is required in CI")
		}
		t.Skip("TEST_DATABASE_URL is not set")
	}
	return u
}
