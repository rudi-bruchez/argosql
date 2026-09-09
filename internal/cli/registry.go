// Package cli is the program's entry point: the command registry, the
// argument parser built on it, and Run, the one function cmd/asq calls.
// No package outside cmd/asq may import it.
package cli

import (
	"context"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/rudi-bruchez/argosql/internal/diagnostics"
	"github.com/rudi-bruchez/argosql/internal/model"
	"github.com/rudi-bruchez/argosql/internal/plan"
	"github.com/rudi-bruchez/argosql/internal/sqlserver"
)

// Flag kinds Validate understands. A flag's Kind decides how its raw
// command-line string is parsed and which of Flag's other fields apply:
// Enum only ever matters for FlagString, Min/Max only for FlagInt64.
const (
	FlagString = "string"
	FlagBool   = "bool"
	FlagInt64  = "int64"
)

// Flag describes one command-line flag as the registry declares it:
// enough for the parser to recognize it, validate its value, and for
// help to render it, without any of that logic living in the command
// itself.
//
// Flag{Name,Kind string; Default any; Min,Max int64} - the shape named
// by the implementation plan - cannot express either an enum (--format
// tsv|json, --by cpu|duration|...) or a mutual exclusion (--no-truncate
// vs --truncate, --hours vs --since/--until). Enum and ExclusiveWith
// exist so every command that needs either rule declares it here, once,
// rather than re-checking it by hand in its own Execute.
//
// Min and Max apply only when both are not simultaneously zero: a flag
// with no declared bound (Min==0 and Max==0, such as --ctx or --db) is
// never range-checked. A flag whose true valid range is exactly [0, 0]
// does not exist in this registry, so this convention never collides
// with a real bound.
type Flag struct {
	Name          string
	Kind          string
	Default       any
	Min, Max      int64
	Enum          []string
	ExclusiveWith []string
}

// flagError builds the code-2 error Validate and the parser return for a
// malformed or out-of-range flag value.
func flagError(name, message string) error {
	return &model.PublicError{Code: 2, Kind: "flag", Message: fmt.Sprintf("--%s: %s", name, message)}
}

// Validate parses raw according to f.Kind, checking it against Enum (for
// FlagString) or against Min/Max (for FlagInt64, when a bound is
// declared - see Flag's doc comment). It does not know about
// ExclusiveWith: that rule depends on which other flags were explicitly
// given on this invocation, which only the parser can see once every
// flag is tokenized - see checkExclusions.
func (f Flag) Validate(raw string) (any, error) {
	switch f.Kind {
	case FlagBool:
		v, err := strconv.ParseBool(raw)
		if err != nil {
			return nil, flagError(f.Name, fmt.Sprintf("%q is not a valid boolean", raw))
		}
		return v, nil
	case FlagInt64:
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return nil, flagError(f.Name, fmt.Sprintf("%q is not a valid integer", raw))
		}
		if f.Min != 0 || f.Max != 0 {
			if n < f.Min || n > f.Max {
				return nil, flagError(f.Name, fmt.Sprintf("%d is out of range [%d, %d]", n, f.Min, f.Max))
			}
		}
		return n, nil
	case FlagString:
		if len(f.Enum) > 0 && !slices.Contains(f.Enum, raw) {
			return nil, flagError(f.Name, fmt.Sprintf("%q is not one of %s", raw, strings.Join(f.Enum, "|")))
		}
		return raw, nil
	default:
		return nil, flagError(f.Name, fmt.Sprintf("internal: unknown flag kind %q", f.Kind))
	}
}

// zeroForKind is the Request value an unset flag falls back to when the
// registry itself declares no Default (ctx, db, config and out-dir: the
// empty string is exactly "not given" for every one of them).
func zeroForKind(kind string) any {
	switch kind {
	case FlagBool:
		return false
	case FlagInt64:
		return int64(0)
	default:
		return ""
	}
}

// Request carries one parsed invocation: the resolved command name,
// connection/output flags, every command-specific flag any registered
// command may need, and Explicit, which remembers which flag names the
// user actually typed - not merely which ones ended up with a non-zero
// value. --no-truncate must be rejected alongside an explicit --truncate
// even when that --truncate repeats its own default (200): a check that
// compared values instead of presence would miss exactly that case, the
// single most likely way a user triggers this conflict.
type Request struct {
	Command, ContextName, Database, ConfigPath, Format, OutDir string
	Object, Table, By, Aggregate, Since, Until                 string
	QueryID, PlanID, MinExecutions                             int64
	TimeoutSeconds, PreviewRows, TruncateRunes, Hours, Top     int
	NoTruncate, IncludeInternal, Summary                       bool
	Explicit                                                   map[string]bool
	// Window is ParseWindow's resolved [Since, Until) for any command
	// that declares the --hours/--since/--until flags (today, "qs top"
	// alone) - resolved once in Parse, before any connection opens (see
	// Parse's own doc comment on why), so Execute consumes it rather
	// than calling ParseWindow a second time against a later, drifted
	// time.Now(). Zero value for every command that does not declare
	// those flags.
	Window diagnostics.Window
}

// Command is one registered command: its table contracts, the flags it
// accepts beyond the global set, how to run it, and what help must be
// able to render about it - examples, output units, required
// permissions and supported engine versions (design spec line 41). A
// registry that did not carry these itself would force help to
// reinvent them by hand for every command, the opposite of a single
// source of truth.
//
// Offline is true only for help: Run must never resolve a config path,
// an out-dir, or open a connection to run it, so that help keeps
// working on a machine with neither configured.
type Command struct {
	Name    string
	Tables  []model.TableSpec
	Flags   []Flag
	Execute func(ctx context.Context, s *sqlserver.Session, req Request, dst model.Sink) error

	Summary     string
	Examples    []string
	Units       []string
	Permissions []string
	Versions    []string
	Offline     bool

	// Positional names one required positional argument the command
	// takes beyond its own words - empty for every command that takes
	// none. What Parse validates that token AS depends on
	// PositionalIsObject, right below: a plain integer (query_id,
	// plan_id's own command) by default, or a two-part schema.object
	// name when PositionalIsObject is true. Either way Parse validates
	// and parses it before any connection opens, the same "syntactic
	// check before connection" ordering already applied to --object and
	// the window flags. Empty means matchCommand's ordinary contract
	// still applies: any bare token left over after the command's own
	// words is an error.
	Positional string

	// PositionalIsObject changes what Positional's one extra bare token
	// means: instead of a positive int64 parsed into req.QueryID, it is
	// a two-part schema.object name, validated exactly like --object
	// already is (sqlserver.ValidateQualifiedName) and parsed into
	// req.Object - "obj table", "obj code", "idx list" and "size
	// table"'s own <schema.name> argument. req.Object is the same field
	// "qs top"'s --object flag fills; the two never collide, because no
	// command in this registry declares both a PositionalIsObject
	// positional and an --object flag.
	PositionalIsObject bool
}

// Registry is the ordered list of every command this build of asq
// knows about. Task 9a's registry holds exactly three: help, info and
// qs status; later tasks append to it, never replace matchCommand's
// exact-name matching with anything resembling a prefix or an
// abbreviation.
type Registry []Command

// globalFlags are accepted by every connecting command (everything but
// help): --ctx, --db, --config, --format, --timeout, --preview,
// --truncate, --no-truncate, --out-dir. --ctx's requiredness is not
// expressed as a Flag field - it is a property of the command, not of
// the flag - see Command.Offline and Parse's check right after matching
// the command.
func globalFlags() []Flag {
	return []Flag{
		{Name: "ctx", Kind: FlagString},
		{Name: "db", Kind: FlagString},
		{Name: "config", Kind: FlagString},
		{Name: "format", Kind: FlagString, Enum: []string{"tsv", "json"}, Default: "tsv"},
		{Name: "timeout", Kind: FlagInt64, Default: int64(30), Min: 1, Max: 300},
		{Name: "preview", Kind: FlagInt64, Default: int64(10), Min: 0, Max: 10000},
		{Name: "truncate", Kind: FlagInt64, Default: int64(200), Min: 1, Max: 10000, ExclusiveWith: []string{"no-truncate"}},
		{Name: "no-truncate", Kind: FlagBool, Default: false, ExclusiveWith: []string{"truncate"}},
		{Name: "out-dir", Kind: FlagString},
	}
}

// NewRegistry builds the registry this build of asq serves: help, info,
// qs status, qs top, qs query, plan, obj table, obj code, idx list and
// size table. help's Execute closes over the finished registry through
// reg, a pointer to the slice NewRegistry is about to return: by the
// time help actually runs, reg has been fully populated by the
// composite literal below, even though help's own entry is built first.
func NewRegistry() Registry {
	reg := make(Registry, 10)
	reg[0] = helpCommand(&reg)
	reg[1] = infoCommand()
	reg[2] = qsStatusCommand()
	reg[3] = qsTopCommand()
	reg[4] = qsQueryCommand()
	reg[5] = planCommand()
	reg[6] = objTableCommand()
	reg[7] = objCodeCommand()
	reg[8] = idxListCommand()
	reg[9] = sizeTableCommand()
	return reg
}

// helpCommandsTableSpec holds one row per registered command: its
// identity and the help-only fields that do not vary in number
// (summary, examples, units, permissions, versions, each flattened to
// a single "; "-joined string, because model.Cell carries scalars
// only). Flags live in helpFlagsTableSpec instead, one row per flag:
// see its own doc comment for why a command's flags cannot join this
// table as just another flattened string column.
var helpCommandsTableSpec = model.TableSpec{
	Name: "commands",
	Columns: []model.Column{
		{Name: "name", SQLType: "NVARCHAR"},
		{Name: "summary", SQLType: "NVARCHAR"},
		{Name: "examples", SQLType: "NVARCHAR"},
		{Name: "units", SQLType: "NVARCHAR"},
		{Name: "permissions", SQLType: "NVARCHAR"},
		{Name: "versions", SQLType: "NVARCHAR"},
	},
}

// helpFlagsTableSpec holds one row per flag accepted by any registered
// command (global flags repeated for every connecting command, plus
// each command's own). A flag's name, kind, default, bounds and enum
// each land in their own column, rather than one "; "-joined cell per
// command the way helpCommandsTableSpec's other list fields do: a
// command that accepts nine global flags plus several of its own -
// "qs top" does, since task 10 added it - pushes a single flattened cell
// well past the project's 200-code-point preview cell limit, which
// truncates help's own inventory by default; the design spec (line 45)
// requires that inventory to carry "parameters, defaults, and
// examples" in full, so it cannot be the one output this project lets
// its own truncation guarantee quietly break. See
// TestHelpOfflineNoCellTruncated.
var helpFlagsTableSpec = model.TableSpec{
	Name: "flags",
	Columns: []model.Column{
		{Name: "command", SQLType: "NVARCHAR"},
		{Name: "flag", SQLType: "NVARCHAR"},
		{Name: "kind", SQLType: "NVARCHAR"},
		{Name: "default", SQLType: "NVARCHAR"},
		{Name: "min", SQLType: "INT"},
		{Name: "max", SQLType: "INT"},
		{Name: "enum", SQLType: "NVARCHAR"},
	},
}

// helpCommand builds the help command. reg is a pointer to the
// in-progress registry slice built by NewRegistry; Execute dereferences
// it only when actually called, by which point NewRegistry has finished
// populating every entry, help's own included.
func helpCommand(reg *Registry) Command {
	return Command{
		Name:     "help",
		Offline:  true,
		Summary:  "List every registered command and every flag it accepts. Works with no configuration file and no connection.",
		Examples: []string{"asq help", "asq help --json"},
		Versions: []string{"2019", "2022"},
		Tables:   []model.TableSpec{helpCommandsTableSpec, helpFlagsTableSpec},
		Flags:    []Flag{{Name: "json", Kind: FlagBool, Default: false}},
		Execute: func(_ context.Context, _ *sqlserver.Session, _ Request, dst model.Sink) error {
			return runHelp(*reg, dst)
		},
	}
}

// runHelp fills dst with one row per command (the commands table) and
// one row per flag any command accepts (the flags table). It never
// touches a file or a connection: everything it reads comes from the
// registry already held in memory.
func runHelp(reg Registry, dst model.Sink) error {
	if err := dst.Begin(helpCommandsTableSpec); err != nil {
		return err
	}
	for _, c := range reg {
		row := []model.Cell{
			c.Name,
			c.Summary,
			strings.Join(c.Examples, "; "),
			strings.Join(c.Units, "; "),
			strings.Join(c.Permissions, "; "),
			strings.Join(c.Versions, "; "),
		}
		if err := dst.Row(row); err != nil {
			return err
		}
	}
	if err := dst.End(true, true); err != nil {
		return err
	}

	if err := dst.Begin(helpFlagsTableSpec); err != nil {
		return err
	}
	for _, c := range reg {
		for _, f := range flagsFor(c) {
			var min, max model.Cell
			if f.Min != 0 || f.Max != 0 {
				min, max = f.Min, f.Max
			}
			row := []model.Cell{
				c.Name,
				f.Name,
				f.Kind,
				f.Default,
				min,
				max,
				strings.Join(f.Enum, "|"),
			}
			if err := dst.Row(row); err != nil {
				return err
			}
		}
	}
	return dst.End(true, true)
}

// flagsFor lists every flag c accepts: global flags first (unless c is
// offline, which accepts none of them), then c's own.
func flagsFor(c Command) []Flag {
	var flags []Flag
	if !c.Offline {
		flags = append(flags, globalFlags()...)
	}
	return append(flags, c.Flags...)
}

// infoCommand registers "info": resolved server/database identity,
// product version, edition, compatibility level, and current
// principal (design spec). Tables is diagnostics.InfoTable itself,
// not a local copy, so help's advertised schema and what
// diagnostics.Info actually writes can never drift apart.
func infoCommand() Command {
	return Command{
		Name:        "info",
		Summary:     "Resolved server/database identity, product version, edition, compatibility level, and current principal.",
		Examples:    []string{"asq --ctx client --db AppDB info"},
		Units:       []string{"compatibility_level: integer"},
		Permissions: []string{"CONNECT"},
		Versions:    []string{"2019", "2022"},
		Tables:      []model.TableSpec{diagnostics.InfoTable},
		Execute: func(ctx context.Context, s *sqlserver.Session, _ Request, dst model.Sink) error {
			return diagnostics.Info(ctx, s, dst)
		},
	}
}

// qsStatusCommand registers "qs status": desired/actual Query Store
// state, decoded read-only reason, capture mode, storage usage/limit,
// retention/interval settings, and stored interval coverage, in the
// design spec's declared table order (status, coverage). Tables is
// diagnostics.StatusTable and diagnostics.CoverageTable themselves, for
// the same reason infoCommand above uses diagnostics.InfoTable.
func qsStatusCommand() Command {
	return Command{
		Name:        "qs status",
		Summary:     "Desired/actual Query Store state, decoded read-only reason, capture mode, storage usage/limit, retention/interval settings, and stored interval coverage.",
		Examples:    []string{"asq --ctx client --db AppDB qs status"},
		Units:       []string{"current_storage_mb, max_storage_mb: MiB", "retention_days: days", "interval_minutes: minutes"},
		Permissions: []string{"VIEW DATABASE STATE"},
		Versions:    []string{"2019", "2022"},
		Tables:      []model.TableSpec{diagnostics.StatusTable, diagnostics.CoverageTable},
		Execute: func(ctx context.Context, s *sqlserver.Session, _ Request, dst model.Sink) error {
			return diagnostics.Status(ctx, s, dst)
		},
	}
}

// qsTopCommand registers "qs top": the exact Query Store ranking of
// queries by total or average CPU, duration, logical reads, or
// executions over a requested window (design spec). Tables is
// diagnostics.TopQueriesTable itself, for the same reason infoCommand
// above uses diagnostics.InfoTable.
//
// --by and --aggregate are validated by a static Enum here, never by
// free text in Execute (design spec: the metric name is one of a fixed
// list); --hours is mutually exclusive with --since/--until
// (ExclusiveWith), matching the design spec's "accept either --hours N
// ... or both --since --until"; --min-executions and --hours declare
// Max explicitly (math.MaxInt64) because Flag.Validate only range-checks
// when at least one of Min/Max is nonzero, and leaving Max at its zero
// value once Min is set would reject every value including the default.
func qsTopCommand() Command {
	return Command{
		Name: "qs top",
		// Names --object and parent_module here, in Summary, rather than
		// as a new descriptive field on Flag: Flag.Description would
		// reopen helpFlagsTableSpec's one-row-per-flag shape that task
		// 9a built specifically so help stops truncating itself, and one
		// design-spec clause (line 61: "Help and output label it
		// parent_module") does not justify reopening that contract.
		Summary: "Rank Query Store queries by total or average CPU, duration, logical reads, or executions over a requested window; --object filters by parent_module.",
		Examples: []string{
			"asq --ctx client --db AppDB qs top --by cpu --hours 24 --top 10",
			"asq --ctx client --db AppDB qs top --by reads --aggregate avg --since 2026-09-01T00:00:00Z --until 2026-09-02T00:00:00Z",
		},
		Units: []string{
			"cpu_total_ms, cpu_avg_ms, duration_total_ms, duration_avg_ms: milliseconds",
			"reads_total, reads_avg: 8-KB logical page reads",
		},
		// sys.query_store_runtime_stats itself requires VIEW DATABASE
		// STATE before 2022 and VIEW DATABASE PERFORMANCE STATE from
		// 2022 on; task 8 measured, against a real server, that VIEW
		// DATABASE STATE implies VIEW DATABASE PERFORMANCE STATE, so
		// granting the former alone suffices on both versions - kept
		// short enough to stay under the project's 200-code-point cell
		// limit (see TestHelpOfflineNoCellTruncated).
		Permissions: []string{
			"VIEW DATABASE STATE (2019)",
			"VIEW DATABASE PERFORMANCE STATE (2022+; implied by VIEW DATABASE STATE)",
		},
		Versions: []string{"2019", "2022"},
		// ranking first, queries second: the design spec's declared
		// table order (design spec line 103 lists "qs top: queries"
		// alone, predating this fix's own "ranking" header table, which
		// must still precede the rows it describes).
		Tables: []model.TableSpec{diagnostics.TopRankingTable, diagnostics.TopQueriesTable},
		Flags: []Flag{
			{Name: "object", Kind: FlagString},
			{Name: "min-executions", Kind: FlagInt64, Default: int64(1), Min: 1, Max: math.MaxInt64},
			{Name: "by", Kind: FlagString, Enum: []string{"cpu", "duration", "reads", "executions"}, Default: "cpu"},
			{Name: "aggregate", Kind: FlagString, Enum: []string{"total", "avg"}, Default: "total"},
			{Name: "hours", Kind: FlagInt64, Default: int64(24), Min: 1, Max: math.MaxInt64, ExclusiveWith: []string{"since", "until"}},
			{Name: "since", Kind: FlagString, ExclusiveWith: []string{"hours"}},
			{Name: "until", Kind: FlagString, ExclusiveWith: []string{"hours"}},
			{Name: "top", Kind: FlagInt64, Default: int64(10), Min: 1, Max: 100},
			{Name: "include-internal", Kind: FlagBool, Default: false},
		},
		Execute: func(ctx context.Context, s *sqlserver.Session, req Request, dst model.Sink) error {
			opts := diagnostics.TopOptions{
				Window:          req.Window,
				By:              req.By,
				Aggregate:       req.Aggregate,
				Object:          req.Object,
				Top:             req.Top,
				MinExecutions:   req.MinExecutions,
				IncludeInternal: req.IncludeInternal,
			}
			return diagnostics.Top(ctx, s, opts, dst)
		},
	}
}

// qsQueryCommand registers "qs query <query_id>": one query's identity
// (parent_module, is_internal_query, query_hash, a text preview and a
// complete exported .sql artifact) plus every recorded plan's
// execution counts and averages over a requested window (design spec:
// "qs query <query_id>": "Identity, parent object when visible, SQL
// preview and full-text artifact, per-plan execution counts and
// averages for the selected window"). Tables is diagnostics.QueryTable
// and diagnostics.PlansTable themselves, for the same reason
// infoCommand above uses diagnostics.InfoTable.
//
// query_id is Positional, never a flag - see Command.Positional's own
// doc comment. --hours/--since/--until are exactly qsTopCommand's own
// window flags, declared again here rather than shared by reference:
// Flag is a plain value type with no notion of "the same flag as
// another command", and each command's Flags slice is what both Parse
// and help walk independently.
func qsQueryCommand() Command {
	return Command{
		Name:       "qs query",
		Positional: "query_id",
		Summary:    "One Query Store query's identity (parent_module, is_internal_query, query_hash, text preview, full-text artifact) and its plans' execution counts/averages over a window; query_id is positional.",
		Examples:   []string{"asq --ctx client --db AppDB qs query 4821 --hours 24"},
		Units: []string{
			"cpu_total_ms, cpu_avg_ms, duration_total_ms, duration_avg_ms: milliseconds",
			"reads_total, reads_avg: 8-KB logical page reads",
		},
		// Same measured implication as qs top's own Permissions field:
		// VIEW DATABASE STATE alone suffices on both versions.
		Permissions: []string{
			"VIEW DATABASE STATE (2019)",
			"VIEW DATABASE PERFORMANCE STATE (2022+; implied by VIEW DATABASE STATE)",
		},
		Versions: []string{"2019", "2022"},
		// window first (fix 1's A1: same header-before-data order as
		// "qs top"'s ranking table, which predates the design spec's
		// own declared table order for "qs query": query, plans).
		Tables: []model.TableSpec{diagnostics.QueryWindowTable, diagnostics.QueryTable, diagnostics.PlansTable},
		Flags: []Flag{
			{Name: "hours", Kind: FlagInt64, Default: int64(24), Min: 1, Max: math.MaxInt64, ExclusiveWith: []string{"since", "until"}},
			{Name: "since", Kind: FlagString, ExclusiveWith: []string{"hours"}},
			{Name: "until", Kind: FlagString, ExclusiveWith: []string{"hours"}},
		},
		Execute: func(ctx context.Context, s *sqlserver.Session, req Request, dst model.Sink) error {
			opts := diagnostics.QueryOptions{ID: req.QueryID, Window: req.Window}
			return diagnostics.Query(ctx, s, opts, dst)
		},
	}
}

// planCommand registers "plan <query_id> --plan-id <id> [--summary]":
// verifies the plan belongs to the query, exports its complete XML to
// a ".sqlplan" artifact, and, only when --summary is given, a bounded
// statement/operators/references/warnings summary of optimizer
// estimates (design spec: "Verify plan belongs to query; export
// complete XML to .sqlplan; optional bounded summary"). Tables is
// plan.StatementTable/plan.OperatorsTable/plan.ReferencesTable/
// plan.WarningsTable themselves, for the same reason infoCommand above
// uses diagnostics.InfoTable - these four are always listed here as
// the command's declared schema even though a run without --summary
// writes none of them; Command.Tables is registry/help metadata, not a
// promise that every run populates every table (help already renders
// "qs status"'s flags the same way regardless of what a given
// invocation actually needs).
//
// query_id is Positional, exactly like "qs query"'s own query_id (see
// Command.Positional's own doc comment); --plan-id has no analogous
// mechanism to lean on - Flag carries no "required" field, since every
// other flag in this registry is genuinely optional - so its
// requiredness is checked directly in Parse, the same way --ctx's is
// (see Parse's own check for cmd.Name == "plan").
func planCommand() Command {
	return Command{
		Name:       "plan",
		Positional: "query_id",
		Summary:    "Export one Query Store plan's complete XML to a .sqlplan artifact; --plan-id is required. --summary adds a bounded statement/operators/references/warnings summary of optimizer estimates.",
		Examples: []string{
			"asq --ctx client --db AppDB plan 4821 --plan-id 9033",
			"asq --ctx client --db AppDB plan 4821 --plan-id 9033 --summary",
		},
		Units: []string{
			"statement.estimated_cost, operators.estimated_subtree_cost: optimizer cost units (unitless, relative)",
		},
		// sys.query_store_plan is read the same way sys.query_store_query
		// and sys.query_store_runtime_stats are elsewhere in this
		// project - same measured implication as qs top's own
		// Permissions field: VIEW DATABASE STATE alone suffices on both
		// versions.
		Permissions: []string{
			"VIEW DATABASE STATE (2019)",
			"VIEW DATABASE PERFORMANCE STATE (2022+; implied by VIEW DATABASE STATE)",
		},
		Versions: []string{"2019", "2022"},
		Tables:   []model.TableSpec{plan.StatementTable, plan.OperatorsTable, plan.ReferencesTable, plan.WarningsTable},
		Flags: []Flag{
			{Name: "plan-id", Kind: FlagInt64, Min: 1, Max: math.MaxInt64},
			{Name: "summary", Kind: FlagBool, Default: false},
		},
		Execute: func(ctx context.Context, s *sqlserver.Session, req Request, dst model.Sink) error {
			return diagnostics.Plan(ctx, s, req.QueryID, req.PlanID, req.Summary, dst)
		},
	}
}

// objTableCommand registers "obj table <schema.name>": obj's ordered
// columns and index structure, plus its approximate row count when
// size permissions allow it (design spec: "obj table"). Tables is
// diagnostics.TableTable/ColumnsTable/IndexesTable themselves, for the
// same reason infoCommand above uses diagnostics.InfoTable - in the
// design spec's own declared order (line 103: "obj table: table,
// columns, indexes").
//
// schema.name is Positional with PositionalIsObject, exactly like
// "obj code"/"idx list"/"size table" below - see Command.Positional's
// own doc comment.
func objTableCommand() Command {
	return Command{
		Name:               "obj table",
		Positional:         "schema.name",
		PositionalIsObject: true,
		Summary:            "Ordered columns (types, length, precision, scale, nullability, identity/computed/default metadata) and index structure for a table; approximate row count when size permissions allow it.",
		Examples:           []string{"asq --ctx client --db AppDB obj table dbo.Orders"},
		Units: []string{
			"max_length: bytes, as sys.columns itself stores it; for nchar/nvarchar this is twice the character count; -1 means MAX",
		},
		// Resolving and reading columns/indexes needs only VIEW
		// DEFINITION and (for a table) SELECT - the "I" bundle's own
		// baseline, without its two 2022-only size-specific grants: this
		// command degrades to an unavailable row count rather than
		// requiring them (design spec line 172).
		Permissions: []string{"VIEW DEFINITION", "SELECT (object-level, for a table)"},
		Versions:    []string{"2019", "2022"},
		Tables:      []model.TableSpec{diagnostics.TableTable, diagnostics.ColumnsTable, diagnostics.IndexesTable},
		Execute: func(ctx context.Context, s *sqlserver.Session, req Request, dst model.Sink) error {
			return diagnostics.Table(ctx, s, req.Object, dst)
		},
	}
}

// objCodeCommand registers "obj code <schema.name>": exports a visible
// module's definition to a .sql artifact, or reports one of the design
// spec's three failure states (encrypted, permission_denied,
// definition_unavailable) purely as the returned error's Kind, never
// as a table row - see diagnostics.Code's own doc comment and
// diagnostics.ModuleTable's, on why no row exists for those three
// cases. Tables still names diagnostics.ModuleTable, for help's
// benefit: a run that succeeds does write exactly this table.
func objCodeCommand() Command {
	return Command{
		Name:               "obj code",
		Positional:         "schema.name",
		PositionalIsObject: true,
		Summary:            "Export a visible module's definition to a .sql artifact, with identity and line count. encrypted/permission_denied/definition_unavailable are reported as an error kind, never a table row.",
		Examples:           []string{"asq --ctx client --db AppDB obj code dbo.EncryptedProc"},
		Permissions:        []string{"VIEW DEFINITION"},
		Versions:           []string{"2019", "2022"},
		Tables:             []model.TableSpec{diagnostics.ModuleTable},
		Execute: func(ctx context.Context, s *sqlserver.Session, req Request, dst model.Sink) error {
			return diagnostics.Code(ctx, s, req.Object, dst)
		},
	}
}

// idxListCommand registers "idx list <schema.name>": every index of an
// object, its ordered keys with direction, included columns, filter,
// uniqueness and disabled state (design spec: "idx list"). Tables is
// diagnostics.IndexesTable itself - the exact same value "obj table"
// declares for its own third table, since Table (table.go) and Indexes
// (indexes.go) share readIndexes, the private reader neither one
// invokes the other's command to reach (design spec: "shares
// implementation with table inspection").
func idxListCommand() Command {
	return Command{
		Name:               "idx list",
		Positional:         "schema.name",
		PositionalIsObject: true,
		Summary:            "Every index of an object: name/type, ordered keys with direction, included columns, filter, uniqueness and disabled state.",
		Examples:           []string{"asq --ctx client --db AppDB idx list dbo.Orders"},
		Permissions:        []string{"VIEW DEFINITION"},
		Versions:           []string{"2019", "2022"},
		Tables:             []model.TableSpec{diagnostics.IndexesTable},
		Execute: func(ctx context.Context, s *sqlserver.Session, req Request, dst model.Sink) error {
			return diagnostics.Indexes(ctx, s, req.Object, dst)
		},
	}
}

// sizeTableCommand registers "size table <schema.name>": approximate
// row count and allocated/used/reserved space, broken down by index
// and allocation type (design spec: "size table"). Tables is
// diagnostics.TableTable and diagnostics.AllocationsTable themselves,
// in the design spec's own declared order (line 103: "size table:
// table, allocations") - the same TableTable value "obj table" opens
// too, so help's advertised schema for that shared header row can
// never drift between the two commands.
//
// Unlike "obj table", this command REQUIRES the size-specific
// permissions (design spec line 172's second half): a row count it
// cannot read fails the whole command at code 4, rather than
// degrading - see diagnostics.Size's own doc comment.
func sizeTableCommand() Command {
	return Command{
		Name:               "size table",
		Positional:         "schema.name",
		PositionalIsObject: true,
		Summary:            "Approximate row count and allocated/used/reserved space for a table, broken down by index_id, partition_number and allocation_type.",
		Examples:           []string{"asq --ctx client --db AppDB size table dbo.Orders"},
		Units: []string{
			"used_pages, reserved_pages: 8-KB pages",
			"used_bytes, reserved_bytes: bytes (pages * 8192); MiB = bytes / 1048576",
		},
		Permissions: []string{
			"VIEW DEFINITION, SELECT (2019)",
			"VIEW DATABASE PERFORMANCE STATE, VIEW SECURITY DEFINITION (2022+, in addition to the above)",
		},
		Versions: []string{"2019", "2022"},
		Tables:   []model.TableSpec{diagnostics.TableTable, diagnostics.AllocationsTable},
		Execute: func(ctx context.Context, s *sqlserver.Session, req Request, dst model.Sink) error {
			return diagnostics.Size(ctx, s, req.Object, dst)
		},
	}
}

// matchCommand finds, among r, the command whose Name exactly equals
// some leading run of bare (non-flag) tokens, joined by single spaces -
// trying the longest possible run first, then shorter ones, so a
// two-word command ("qs status") is preferred over a one-word command
// that happens to share its first token, and a bare token is never
// matched against a command name by a prefix or an abbreviation: only
// exact, whole-word equality ever resolves a command. It reports how
// many leading tokens of bare the match consumed, so the caller can
// treat anything after them as extra positional arguments.
func (r Registry) matchCommand(bare []string) (*Command, int, error) {
	if len(bare) == 0 {
		return nil, 0, &model.PublicError{Code: 2, Kind: "command", Message: "no command given"}
	}
	maxWords := 0
	for i := range r {
		if n := len(strings.Fields(r[i].Name)); n > maxWords {
			maxWords = n
		}
	}
	limit := maxWords
	if len(bare) < limit {
		limit = len(bare)
	}
	for n := limit; n >= 1; n-- {
		candidate := strings.Join(bare[:n], " ")
		for i := range r {
			if r[i].Name == candidate {
				return &r[i], n, nil
			}
		}
	}
	return nil, 0, &model.PublicError{Code: 2, Kind: "unknown_command", Message: fmt.Sprintf("unknown command: %s", strings.Join(bare, " "))}
}

// flagSuperset merges every flag this registry's commands declare,
// global flags included, into one name-keyed table - used only while
// tokenizing, to decide whether a "--name" the parser has not yet been
// able to attribute to a specific command takes a following value
// (anything but FlagBool does) before the command itself is known (see
// Parse's doc comment on why that order matters). The first definition
// of a given name wins; today no two commands in this registry define
// the same flag name with a different Kind.
func (r Registry) flagSuperset() map[string]Flag {
	all := map[string]Flag{}
	for _, f := range globalFlags() {
		all[f.Name] = f
	}
	for _, c := range r {
		for _, f := range c.Flags {
			if _, ok := all[f.Name]; !ok {
				all[f.Name] = f
			}
		}
	}
	return all
}

// checkExclusions reports the first ExclusiveWith conflict among flags
// actually given, per explicit - true regardless of which of the two
// conflicting names flags happens to declare the relationship on.
func checkExclusions(flags []Flag, explicit map[string]bool) error {
	for _, f := range flags {
		if !explicit[f.Name] {
			continue
		}
		for _, other := range f.ExclusiveWith {
			if explicit[other] {
				return &model.PublicError{
					Code:    2,
					Kind:    "flag_conflict",
					Message: fmt.Sprintf("--%s cannot be combined with --%s", f.Name, other),
				}
			}
		}
	}
	return nil
}
