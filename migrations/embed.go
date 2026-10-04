// Package migrations holds the database schema of server in golang-migrate format.
package migrations

import (
	"embed"
	"io/fs"
	"strconv"
	"strings"
)

// FS holds the migration files.
//
//go:embed *.sql
var FS embed.FS

// Latest returns the highest migration version: the schema version this binary expects.
func Latest() uint {
	names, err := fs.Glob(FS, "*.up.sql")
	if err != nil {
		panic(err) // the pattern is constant
	}
	var latest uint
	for _, name := range names {
		prefix, _, _ := strings.Cut(name, "_")
		v, err := strconv.ParseUint(prefix, 10, 64)
		if err != nil {
			panic("migrations: file without a version prefix: " + name)
		}
		latest = max(latest, uint(v))
	}
	return latest
}
