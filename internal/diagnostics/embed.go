package diagnostics

import _ "embed"

// infoQuery, healthQuery and coverageQuery are this package's three
// embedded SQL statements (see sql/*.sql for each one's own doc
// comment on what it reads and why). Embedding them here, rather than
// as a literal string in info.go/health.go, keeps the SQL text
// reviewable and editable as plain .sql files while still shipping
// inside the compiled binary - curated, embedded queries only, per the
// design spec's "no arbitrary SQL command" constraint.
var (
	//go:embed sql/info.sql
	infoQuery string

	//go:embed sql/health.sql
	healthQuery string

	//go:embed sql/coverage.sql
	coverageQuery string
)
