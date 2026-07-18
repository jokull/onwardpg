// Command pgmiginventory deterministically indexes the executable API corpus
// from the pinned pgmig checkout. It is a development/test tool; onwardpg
// production binaries do not import or execute pgmig code.
package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

const pinnedCommit = "d2cccb6886bfb0b6ad0649bbe1430a9ab57ae983"

type matrix struct {
	SchemaVersion   int          `json:"schema_version"`
	Reference       reference    `json:"reference"`
	InventoryStatus string       `json:"inventory_status"`
	SourceFiles     []sourceFile `json:"source_files"`
	Scenarios       []scenario   `json:"scenarios"`
}

type reference struct {
	Repository   string `json:"repository"`
	Commit       string `json:"commit"`
	License      string `json:"license"`
	ScenarioRoot string `json:"scenario_root"`
}

type sourceFile struct {
	Path      string `json:"path"`
	SHA256    string `json:"sha256"`
	Scenarios int    `json:"scenarios"`
}

type scenario struct {
	ID             string   `json:"id"`
	Area           string   `json:"area"`
	Family         string   `json:"family"`
	Name           string   `json:"name"`
	PGMigTest      string   `json:"pgmig_test"`
	SourceLine     int      `json:"source_line"`
	MinPostgres    int      `json:"min_postgres"`
	PGMigOutcome   string   `json:"pgmig_outcome"`
	Classification string   `json:"classification"`
	Workstream     string   `json:"workstream"`
	Tags           []string `json:"tags"`
	OnwardPGTests  []string `json:"onwardpg_tests,omitempty"`
	Notes          string   `json:"notes,omitempty"`
}

type familyPolicy struct {
	Workstream string
	Tests      []string
}

var policies = map[string]familyPolicy{
	"constraint":        {"constraints_indexes", []string{"internal/graphplan#TestConstraintAndIndexRebuildConvergeOnPostgreSQL"}},
	"general":           {"catalog_guards", []string{"internal/source#TestLoadGraphBlocksPreviouslyBlindCatalogFamilies"}},
	"materialized_view": {"materialized_views", []string{"internal/graphplan#TestMaterializedViewCreateRebuildAndDropConvergeOnPostgreSQL"}},
	"routine":           {"routines_triggers", []string{"internal/graphplan#TestRoutineAndTriggerLifecycleConvergesOnPostgreSQL"}},
	"schema":            {"schemas_extensions", []string{"internal/graphplan#TestCreatePlanConvergesOnPostgreSQL"}},
	"sequence":          {"sequences", []string{"internal/graphplan#TestSequenceParameterUpdateConvergesOnPostgreSQL"}},
	"table":             {"tables_columns", []string{"internal/graphplan#TestMutationPlanConvergesOnPostgreSQL"}},
	"type":              {"type_system", []string{"internal/graphplan#TestEnumCreateAndDropConvergeOnPostgreSQL"}},
	"view":              {"views", []string{"internal/graphplan#TestViewCreateAndReplaceConvergeOnPostgreSQL"}},
}

var testDefinition = regexp.MustCompile(`^(?:async )?def (test_[A-Za-z0-9_]+)\s*\(`)

func main() {
	source := flag.String("source", ".pgmig", "path to the pinned pgmig checkout")
	output := flag.String("output", "references/pgmig-d2cccb6.json", "output inventory path")
	flag.Parse()

	if err := run(*source, *output); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(source, output string) error {
	commit, err := exec.Command("git", "-C", source, "rev-parse", "HEAD").Output()
	if err != nil {
		return fmt.Errorf("read pgmig checkout commit: %w", err)
	}
	if actual := strings.TrimSpace(string(commit)); actual != pinnedCommit {
		return fmt.Errorf("pgmig checkout is %s; expected %s", actual, pinnedCommit)
	}

	root := filepath.Join(source, "tests", "_api")
	var files []string
	err = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() && strings.HasPrefix(info.Name(), "test_") && strings.HasSuffix(info.Name(), ".py") {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		return err
	}
	sort.Strings(files)

	result := matrix{
		SchemaVersion: 1,
		Reference: reference{
			Repository:   "https://github.com/Apakottur/pgmig",
			Commit:       pinnedCommit,
			License:      "MIT",
			ScenarioRoot: "tests/_api",
		},
		InventoryStatus: "verified",
	}
	seen := make(map[string]bool)
	for _, path := range files {
		entries, receipt, err := inspectFile(source, root, path)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if seen[entry.ID] {
				return fmt.Errorf("duplicate pgmig scenario id %q", entry.ID)
			}
			seen[entry.ID] = true
		}
		result.SourceFiles = append(result.SourceFiles, receipt)
		result.Scenarios = append(result.Scenarios, entries...)
	}
	if len(result.Scenarios) == 0 {
		return fmt.Errorf("no pgmig API scenarios found in %s", root)
	}

	encoded, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	if err := os.MkdirAll(filepath.Dir(output), 0o755); err != nil {
		return err
	}
	return os.WriteFile(output, encoded, 0o644)
}

func inspectFile(source, root, path string) ([]scenario, sourceFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, sourceFile{}, err
	}
	relRoot, err := filepath.Rel(root, path)
	if err != nil {
		return nil, sourceFile{}, err
	}
	relRoot = filepath.ToSlash(relRoot)
	parts := strings.Split(relRoot, "/")
	if len(parts) < 2 {
		return nil, sourceFile{}, fmt.Errorf("pgmig test %s is not grouped by API area", relRoot)
	}
	area := parts[0]
	policy, exists := policies[area]
	if !exists {
		return nil, sourceFile{}, fmt.Errorf("pgmig API area %q has no policy", area)
	}
	family := strings.TrimSuffix(filepath.Base(path), ".py")
	family = strings.TrimPrefix(family, "test_")

	lines, err := scanLines(data)
	if err != nil {
		return nil, sourceFile{}, err
	}
	type start struct {
		line int
		name string
	}
	var starts []start
	for i, line := range lines {
		match := testDefinition.FindStringSubmatch(line)
		if len(match) == 2 {
			starts = append(starts, start{line: i, name: match[1]})
		}
	}
	var entries []scenario
	for i, item := range starts {
		end := len(lines)
		if i+1 < len(starts) {
			end = starts[i+1].line
		}
		body := strings.Join(lines[item.line:end], "\n")
		relSource, err := filepath.Rel(source, path)
		if err != nil {
			return nil, sourceFile{}, err
		}
		relSource = filepath.ToSlash(relSource)
		entry := scenario{
			ID:             scenarioID(relRoot, item.name),
			Area:           area,
			Family:         family,
			Name:           item.name,
			PGMigTest:      relSource + "#" + item.name,
			SourceLine:     item.line + 1,
			MinPostgres:    minimumPostgres(relRoot, body),
			PGMigOutcome:   upstreamOutcome(body),
			Classification: "implemented_unverified",
			Workstream:     policy.Workstream,
			Tags:           scenarioTags(area, family, item.name),
			OnwardPGTests:  append([]string(nil), policy.Tests...),
			Notes:          "Family-level onwardpg support exists; this exact pgmig scenario still needs direct convergence or rejection evidence.",
		}
		classify(&entry, relRoot)
		entries = append(entries, entry)
	}
	digest := sha256.Sum256(data)
	return entries, sourceFile{Path: filepath.ToSlash(filepath.Join("tests", "_api", relRoot)), SHA256: hex.EncodeToString(digest[:]), Scenarios: len(entries)}, nil
}

func scanLines(data []byte) ([]string, error) {
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	// SQL fixtures can make a Python source line larger than Scanner's default.
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	var lines []string
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	return lines, scanner.Err()
}

func scenarioID(rel, name string) string {
	base := strings.TrimSuffix(rel, ".py")
	base = strings.ReplaceAll(base, "/", ".")
	return base + "." + name
}

func upstreamOutcome(body string) string {
	if strings.Contains(body, "assert_unsupported(") || strings.Contains(body, "PgmigUnsupportedError") || strings.Contains(body, "PgmigApiError") {
		return "rejects"
	}
	return "converges"
}

func minimumPostgres(rel, body string) int {
	if strings.Contains(rel, "test_temporal_constraint.py") || strings.Contains(body, "Postgres 18") {
		return 18
	}
	return 15
}

func scenarioTags(area, family, name string) []string {
	tags := []string{"area:" + area, "family:" + family}
	for _, tag := range []string{"create", "add", "drop", "remove", "rename", "change", "recreate", "comment", "dependency", "unsupported", "unchanged"} {
		if strings.Contains(name, "_"+tag) || strings.Contains(name, tag+"_") {
			tags = append(tags, tag)
		}
	}
	sort.Strings(tags)
	return compact(tags)
}

func compact(values []string) []string {
	result := values[:0]
	for _, value := range values {
		if len(result) == 0 || result[len(result)-1] != value {
			result = append(result, value)
		}
	}
	return result
}

func classify(entry *scenario, rel string) {
	name := entry.Name
	rejection := func(note string) {
		entry.Classification = "intentional_rejection"
		entry.Notes = note
	}
	handoff := func(note string, tests ...string) {
		entry.Classification = "needs_handoff"
		entry.Notes = note
		entry.OnwardPGTests = tests
	}
	verified := func(note string, tests ...string) {
		entry.Classification = "verified"
		entry.Notes = note
		entry.OnwardPGTests = tests
	}

	if entry.PGMigOutcome == "rejects" {
		rejection("Both tools must make this unsupported boundary explicit; scenario-specific onwardpg rejection evidence is still required.")
	}

	switch {
	case strings.Contains(rel, "type/test_domain.py"):
		entry.Workstream = "type_system"
		if entry.PGMigOutcome == "converges" {
			verified("Domain create/drop, defaults, nullability, named checks, comments, and unchanged state converge on real PostgreSQL.", "internal/graphplan#TestDomainLifecycleConvergesOnPostgreSQL", "internal/graphplan#TestDomainDependencyOrderingConvergesOnPostgreSQL")
		} else {
			entry.Notes = "Domain base-type changes remain an explicit typed rejection and are covered by real-PostgreSQL evidence."
			entry.OnwardPGTests = []string{"internal/graphplan#TestDomainLifecycleConvergesOnPostgreSQL"}
		}
	case strings.Contains(rel, "type/test_composite_type.py"):
		entry.Workstream = "type_system"
		if entry.PGMigOutcome == "converges" {
			verified("Composite create/drop, empty types, comments, attribute changes, CASCADE propagation, and nested/array dependency ordering converge on real PostgreSQL.", "internal/graphplan#TestCompositeTypeLifecycleConvergesOnPostgreSQL", "internal/graphplan#TestCompositeTypeDependenciesConvergeOnPostgreSQL")
		} else {
			entry.Notes = "Composite attribute reorder remains an explicit typed rejection with real-PostgreSQL evidence."
			entry.OnwardPGTests = []string{"internal/graphplan#TestCompositeTypeLifecycleConvergesOnPostgreSQL"}
		}
	case strings.Contains(rel, "type/test_range_type.py") || strings.Contains(name, "range_type"):
		entry.Workstream = "type_system"
		if entry.PGMigOutcome == "converges" {
			verified("Range create/drop, subtype options, comments, unchanged state, table dependencies, and property-changing recreation converge on real PostgreSQL.", "internal/graphplan#TestRangeTypeLifecycleConvergesOnPostgreSQL")
		}
	case strings.Contains(rel, "type/test_enum.py") && (strings.Contains(name, "rename_value") || strings.Contains(name, "rename_multiple")):
		entry.Workstream = "enum_rewrites"
		verified("Pure positional enum label renames use ALTER TYPE RENAME VALUE and preserve dependent data on real PostgreSQL.", "internal/graphplan#TestEnumValueRenameConvergesOnPostgreSQL")
	case strings.Contains(rel, "type/test_enum.py") && name == "test_enum_create_empty":
		verified("Empty enum creation and live catalog re-introspection converge on real PostgreSQL.", "internal/graphplan#TestEnumCommentsAndEmptyEnumConvergeOnPostgreSQL")
	case strings.Contains(rel, "type/test_enum") && (strings.Contains(name, "enum_rename_and_insert") || strings.Contains(name, "value_removal") || strings.Contains(name, "value_reorder") || strings.Contains(name, "enum_rewrite")):
		entry.Workstream = "enum_rewrites"
		if entry.PGMigOutcome == "converges" {
			verified("Confirmed enum rewrites recreate the type, retype unchanged scalar/array columns through text, restore defaults/comments, preserve retained data, and converge on real PostgreSQL.", "internal/graphplan#TestEnumRewriteConvergesOnPostgreSQL")
		} else {
			entry.Notes = "Generated, indexed, constrained, domain-mediated, and view-read enum dependents remain explicit typed refusals with real-PostgreSQL evidence."
			entry.OnwardPGTests = []string{"internal/graphplan#TestEnumRewriteRejectsUnsafeDependentsOnPostgreSQL"}
		}
	case strings.Contains(rel, "type/test_enum.py") && strings.Contains(name, "comment"):
		verified("Enum comment create, add, change, removal, and unchanged behavior converges on real PostgreSQL.", "internal/graphplan#TestEnumCommentsAndEmptyEnumConvergeOnPostgreSQL")
	case strings.Contains(rel, "type/test_enum.py"):
		verified("Labeled enum create/drop, append/positional insertion, unchanged state, and dependency-first typed columns converge on real PostgreSQL.", "internal/graphplan#TestEnumCreateAndDropConvergeOnPostgreSQL", "internal/graphplan#TestEnumBasicLifecycleConvergesOnPostgreSQL")
	case strings.Contains(rel, "routine/test_view_trigger.py") && strings.Contains(name, "comment"):
		entry.Workstream = "trigger_parity"
		verified("View-trigger comment add/change/removal state is typed and converges on real PostgreSQL.", "internal/graphplan#TestTriggerCommentsConvergeOnPostgreSQL")
	case strings.Contains(rel, "routine/test_view_trigger.py"):
		entry.Workstream = "trigger_parity"
		verified("INSTEAD OF trigger create/drop/recreate/rename/unchanged behavior and survival across identity-preserving view replacement converge on real PostgreSQL.", "internal/graphplan#TestViewTriggerLifecycleConvergesOnPostgreSQL")
	case strings.Contains(rel, "routine/test_trigger.py") && strings.Contains(name, "constraint_trigger"):
		entry.Workstream = "trigger_parity"
		verified("Constraint-trigger create, unchanged, deferrability replacement, rename, and approved drop converge on real PostgreSQL.", "internal/graphplan#TestConstraintTriggerLifecycleConvergesOnPostgreSQL")
	case strings.Contains(rel, "routine/test_trigger.py") && strings.Contains(name, "comment"):
		entry.Workstream = "trigger_parity"
		if strings.Contains(name, "rename_clears_comment") {
			verified("A confirmed trigger rename explicitly clears a source-only comment and converges.", "internal/graphplan#TestTriggerRenameConvergesOnPostgreSQL")
		} else {
			verified("Table-trigger comment add/change/removal/unchanged state is typed and converges on real PostgreSQL.", "internal/graphplan#TestTriggerCommentsConvergeOnPostgreSQL")
		}
	case strings.Contains(rel, "routine/test_trigger.py") && (strings.Contains(name, "trigger_disabled") || strings.Contains(name, "trigger_reenabled") || strings.Contains(name, "trigger_enable_replica") || strings.Contains(name, "trigger_enable_always") || strings.Contains(name, "trigger_state_unchanged") || strings.Contains(name, "trigger_create_disabled") || strings.Contains(name, "trigger_recreated_preserves_disabled_state")):
		verified("All PostgreSQL trigger enable states, unchanged state, create-state fixup, and recreate-state restoration converge on real PostgreSQL.", "internal/graphplan#TestTriggerEnableStatesConvergeOnPostgreSQL")
	case strings.Contains(rel, "routine/test_trigger.py") && strings.Contains(name, "trigger_renamed_preserves_disabled_state"):
		verified("A confirmed trigger rename retains its disabled catalog state and converges.", "internal/graphplan#TestTriggerRenameConvergesOnPostgreSQL")
	case strings.Contains(rel, "routine/test_trigger.py") && (name == "test_trigger_create" || name == "test_trigger_drop"):
		verified("Ordinary trigger create/drop ordering with its routine converges on real PostgreSQL.", "internal/graphplan#TestRoutineAndTriggerLifecycleConvergesOnPostgreSQL")
	case strings.Contains(rel, "routine/test_trigger.py") && name == "test_trigger_rename":
		verified("A fingerprint-bound ordinary trigger rename converges on real PostgreSQL.", "internal/graphplan#TestTriggerRenameConvergesOnPostgreSQL")
	case strings.Contains(rel, "routine/test_trigger.py") && name == "test_trigger_definition_changed":
		verified("Trigger definition replacement restores the desired enable state and converges.", "internal/graphplan#TestTriggerEnableStatesConvergeOnPostgreSQL")
	case strings.Contains(rel, "routine/test_trigger.py") && name == "test_trigger_unchanged":
		verified("An unchanged trigger definition and enable state replan to no work.", "internal/graphplan#TestTriggerEnableStatesConvergeOnPostgreSQL")
	case strings.Contains(rel, "routine/test_trigger.py") && (name == "test_trigger_internal_ignored" || strings.Contains(name, "trigger_on_partitioned_parent")):
		verified("Foreign-key internal triggers and partition-cloned triggers are filtered while the single partition-parent trigger create/drop lifecycle converges on real PostgreSQL.", "internal/graphplan#TestRoutineOverloadsProceduresAndPartitionTriggersConvergeOnPostgreSQL")
	case strings.Contains(rel, "routine/test_function_deps.py"):
		entry.Workstream = "routine_dependencies"
		if entry.PGMigOutcome == "converges" {
			verified("Catalog-proven routine/default/check/index dependencies, chain/diamond drop order, and bounded return-type recreation converge on real PostgreSQL.", "internal/graphplan#TestRoutineDependenciesAndReturnTypeChangesConvergeOnPostgreSQL")
		} else {
			entry.Classification = "intentional_rejection"
			entry.Notes = "Dropped or changed dependents, routine-on-routine return changes, and circular dropped-relation shapes remain explicit typed refusals with real-PostgreSQL evidence."
			entry.OnwardPGTests = []string{"internal/graphplan#TestRoutineDependencyChangesRejectUnsafeShapesOnPostgreSQL"}
		}
	case strings.Contains(rel, "routine/test_function.py") && strings.Contains(name, "return_type_change"):
		verified("Function return identity is typed; a return-type change drops and recreates the function and converges on real PostgreSQL.", "internal/graphplan#TestRoutineDependenciesAndReturnTypeChangesConvergeOnPostgreSQL")
	case strings.Contains(rel, "routine/test_function.py"):
		if entry.PGMigOutcome == "converges" {
			verified("Function/procedure create/drop, body replacement, overload addition, unchanged state, and function comments converge on real PostgreSQL.", "internal/graphplan#TestRoutineOverloadsProceduresAndPartitionTriggersConvergeOnPostgreSQL")
		} else {
			entry.Classification = "intentional_rejection"
			entry.Notes = "User-defined aggregates remain an explicit typed refusal rather than being silently omitted by routine introspection."
			entry.OnwardPGTests = []string{"internal/source#TestLoadGraphBlocksPreviouslyBlindCatalogFamilies"}
		}
	case strings.Contains(rel, "view/test_view_column_dependencies.py"):
		entry.Classification = "needs_handoff"
		entry.Notes = "Column type changes retain onwardpg's rolling-deployment handoff; the editable plan names the exact direct/transitive ordinary-view dependency closure and excludes views over unchanged columns."
		entry.OnwardPGTests = []string{"internal/graphplan#TestViewColumnTypeChangeHandoffPreservesDependencyScopeOnPostgreSQL"}
	case strings.Contains(rel, "view/test_view_dependencies.py"):
		verified("View-on-view create/drop ordering, transitive and cross-schema dependency chains, definition replacement, and unchanged state converge on real PostgreSQL.", "internal/graphplan#TestViewOptionsAndDependencyChainsConvergeOnPostgreSQL")
	case strings.Contains(rel, "view/test_view_options.py"):
		verified("View reloption create/add/remove/unchanged/order normalization and cascaded check options converge on real PostgreSQL.", "internal/graphplan#TestViewOptionsAndDependencyChainsConvergeOnPostgreSQL")
	case strings.Contains(rel, "view/test_view.py"):
		verified("Ordinary view create/drop/unchanged/definition replacement and comments converge on real PostgreSQL.", "internal/graphplan#TestViewCreateAndReplaceConvergeOnPostgreSQL", "internal/graphplan#TestViewOptionsAndDependencyChainsConvergeOnPostgreSQL")
	case strings.Contains(rel, "general/test_comment_convergence.py") && strings.Contains(name, "function_return_type"):
		verified("Return-type recreation reapplies an identical routine comment and converges on real PostgreSQL.", "internal/graphplan#TestRoutineDependenciesAndReturnTypeChangesConvergeOnPostgreSQL")
	case strings.Contains(rel, "general/test_comment_convergence.py") && (strings.Contains(name, "index_redefinition") || strings.Contains(name, "constraint_redefinition")):
		verified("Index and constraint rebuilds restore identical comments after replacement and converge on real PostgreSQL.", "internal/graphplan#TestConstraintAndIndexRebuildConvergeOnPostgreSQL")
	case strings.Contains(rel, "general/test_sanity.py"):
		verified("Identical empty catalog graphs produce a planned no-op after real-PostgreSQL cleanup.", "internal/graphplan#TestSchemaAndSearchPathConvergeOnPostgreSQL")
	case strings.Contains(rel, "general/test_unsupported_type.py") && strings.Contains(name, "view_return_rule"):
		verified("Ordinary view creation ignores PostgreSQL's automatic _RETURN rule and converges on real PostgreSQL.", "internal/graphplan#TestViewCreateAndReplaceConvergeOnPostgreSQL")
	case strings.Contains(rel, "general/test_unsupported_type.py") && (strings.Contains(name, "rls_policy") || strings.Contains(name, "rls_enabled")):
		verified("Onward models row-security policy and enablement state directly, with authorized lifecycle convergence on real PostgreSQL.", "internal/graphplan#TestRowSecurityPoliciesAndTablePrivilegesConvergeOnPostgreSQL")
	case strings.Contains(rel, "general/test_unsupported_type.py") && strings.Contains(name, "enum_array_type"):
		verified("Enum creation filters PostgreSQL's implicit array type and converges on real PostgreSQL.", "internal/graphplan#TestEnumCreateAndDropConvergeOnPostgreSQL")
	case strings.Contains(rel, "sequence/test_sequence_owner.py"):
		verified("Manually owned sequence create/drop/add/remove/retarget/unchanged behavior, dependency-cascade drops, and identity/serial exclusion rules converge on real PostgreSQL.", "internal/graphplan#TestSequenceOwnedByTransitionsConvergeOnPostgreSQL")
	case strings.Contains(rel, "sequence/test_sequence.py"):
		verified("Sequence create/drop, all parameter and type changes, cycle toggles, comments, combined alterations, and unchanged state converge on real PostgreSQL.", "internal/graphplan#TestSequenceParameterUpdateConvergesOnPostgreSQL")
	case strings.Contains(rel, "sequence/test_sequence_persistence.py"):
		verified("Logged/unlogged sequence creation, both persistence transitions, and unchanged state converge on real PostgreSQL.", "internal/graphplan#TestSequencePersistenceConvergesOnPostgreSQL")
	case strings.Contains(rel, "schema/test_schema.py"):
		verified("Schema create/drop and comments converge on real PostgreSQL.", "internal/graphplan#TestSchemaAndSearchPathConvergeOnPostgreSQL")
	case strings.Contains(rel, "schema/test_search_path.py"):
		verified("Catalog introspection is invariant under an empty connection search_path, and user-defined column types remain schema-qualified in executable plans.", "internal/graphplan#TestSchemaAndSearchPathConvergeOnPostgreSQL")
	case strings.Contains(rel, "table/test_table_ownership.py"):
		verified("Table ownership changes require explicit authorization, unchanged state is stable, and table_owner selectors suppress owner differences; all converge on real PostgreSQL.", "internal/graphplan#TestTableOwnershipChangesConvergeOnPostgreSQL")
	case strings.Contains(rel, "table/test_table_comment.py"):
		verified("Table comment create/add/change/remove/unchanged and quoted-literal behavior converge on real PostgreSQL.", "internal/graphplan#TestCommentTransitionsConvergeOnPostgreSQL")
	case strings.Contains(rel, "table/test_table_column_comment.py"):
		verified("Column comments on table/column creation plus add/change/remove/unchanged and quoted-literal behavior converge on real PostgreSQL.", "internal/graphplan#TestCommentTransitionsConvergeOnPostgreSQL")
	case strings.Contains(rel, "table/test_table_persistence.py"):
		verified("Unlogged table creation, both persistence transitions, unchanged state, and partition-child persistence changes converge on real PostgreSQL.", "internal/graphplan#TestTableLifecycleAndPersistenceConvergeOnPostgreSQL")
	case strings.Contains(rel, "table/test_table.py"):
		verified("Ordinary, quoted-identifier, and zero-column table create/drop/unchanged behavior converges on real PostgreSQL.", "internal/graphplan#TestTableLifecycleAndPersistenceConvergeOnPostgreSQL")
	case strings.Contains(rel, "table/test_table_replica_identity.py"):
		verified("DEFAULT, FULL, NOTHING, and USING INDEX replica identities are typed with dependency-safe index ordering and converge on real PostgreSQL.", "internal/graphplan#TestReplicaIdentityConvergesOnPostgreSQL")
	case strings.Contains(rel, "table/test_table_column_generated.py") && (strings.Contains(name, "virtual") || strings.Contains(name, "expression_change")):
		verified("Stored generated-expression rebuilds converge across PostgreSQL 15–18; PostgreSQL 18 virtual create/add/drop and in-place expression changes also converge.", "internal/graphplan#TestGeneratedColumnExpressionChangesConvergeOnPostgreSQL")
	case strings.Contains(rel, "table/test_table_column_generated.py"):
		if entry.PGMigOutcome == "converges" {
			verified("Stored generated-column create/add/drop, NOT NULL rendering, direct type changes without USING, and unchanged expression identity converge on real PostgreSQL.", "internal/graphplan#TestGeneratedColumnCreateAndDropExpressionConvergeOnPostgreSQL", "internal/graphplan#TestGeneratedColumnExpressionChangesConvergeOnPostgreSQL")
		} else {
			entry.Notes = "Generated-ness changes remain an explicit typed refusal; PostgreSQL has no in-place ADD GENERATED form."
			entry.OnwardPGTests = []string{"internal/graphplan#TestGeneratedColumnExpressionChangesConvergeOnPostgreSQL"}
		}
	case strings.Contains(rel, "table/test_table_column_collation.py") && ((strings.Contains(name, "changed") && !strings.Contains(name, "unchanged")) || strings.Contains(name, "removed")):
		verified("Same-type explicit collation changes and reset-to-default transitions use reviewed ALTER COLUMN TYPE operations and converge on real PostgreSQL.", "internal/graphplan#TestColumnCollationChangesConvergeOnPostgreSQL")
	case strings.Contains(rel, "table/test_table_column_collation.py"):
		verified("Explicit non-default collation creation and unchanged-state normalization converge on real PostgreSQL.", "internal/graphplan#TestColumnCollationChangesConvergeOnPostgreSQL")
	case strings.Contains(rel, "schema/test_extension.py") && strings.Contains(name, "set_schema"):
		entry.Workstream = "typed_attributes"
		verified("Identity-preserving extension schema moves use ALTER EXTENSION SET SCHEMA and converge on real PostgreSQL.", "internal/graphplan#TestExtensionSchemaMoveConvergesOnPostgreSQL")
	case strings.Contains(rel, "schema/test_extension.py") && strings.Contains(name, "ignore_extension_version"):
		entry.Workstream = "tooling"
		verified("A repeatable exact-name option suppresses matching extension version changes while nonmatching names still update and converge on real PostgreSQL.", "internal/graphplan#TestExtensionVersionIgnoreBehaviorOnPostgreSQL")
	case strings.Contains(rel, "schema/test_extension.py") && strings.Contains(name, "comment"):
		verified("Custom extension comments are typed while package defaults are normalized; comment changes converge on real PostgreSQL.", "internal/graphplan#TestExtensionCommentsConvergeOnPostgreSQL")
	case strings.Contains(rel, "schema/test_extension.py"):
		verified("Extension create/drop/version lifecycle, atomic member filtering, and dependency-safe drops after consuming tables converge on real PostgreSQL.", "internal/graphplan#TestExtensionCreateConvergesOnPostgreSQL", "internal/graphplan#TestExtensionDropConvergesOnPostgreSQL", "internal/graphplan#TestExtensionVersionIgnoreBehaviorOnPostgreSQL", "internal/graphplan#TestExtensionAtomicMembersAndTypeDependenciesConvergeOnPostgreSQL")
	case strings.Contains(rel, "constraint/test_temporal_constraint.py"):
		entry.Workstream = "typed_attributes"
		verified("PostgreSQL 18 WITHOUT OVERLAPS primary/unique constraints and PERIOD foreign keys preserve catalog definitions, dependency ordering, rename/rebuild/drop behavior, and unchanged state.", "internal/graphplan#TestTemporalAndEnforcementConstraintsConvergeOnPostgreSQL")
	case strings.Contains(rel, "constraint/test_constraint.py") && (strings.Contains(name, "not_enforced") || strings.Contains(name, "enforcement_change")):
		entry.Workstream = "typed_attributes"
		verified("PostgreSQL 18 NOT ENFORCED check constraints preserve catalog syntax and converge through add, enforcement rebuild, and unchanged states.", "internal/graphplan#TestTemporalAndEnforcementConstraintsConvergeOnPostgreSQL")
	case strings.Contains(rel, "constraint/test_foreign_key.py") && strings.Contains(name, "enforced"):
		entry.Workstream = "typed_attributes"
		verified("PostgreSQL 18 NOT ENFORCED foreign keys preserve catalog syntax and converge through add, both enforcement transitions, and unchanged state.", "internal/graphplan#TestTemporalAndEnforcementConstraintsConvergeOnPostgreSQL")
	case strings.Contains(rel, "constraint/") && strings.Contains(name, "constraint_rename"):
		verified("Fingerprint-confirmed ordinary constraint renames preserve constraint and backing-index identity and converge on real PostgreSQL.", "internal/graphplan#TestConstraintRenamesConvergeOnPostgreSQL")
	case strings.Contains(rel, "constraint/test_foreign_key.py") && strings.Contains(name, "foreign_key_rename"):
		verified("Fingerprint-confirmed foreign-key renames converge on real PostgreSQL.", "internal/graphplan#TestConstraintRenamesConvergeOnPostgreSQL")
	case strings.Contains(rel, "constraint/test_constraint_nulls_not_distinct.py"):
		verified("PostgreSQL 15+ NULLS NOT DISTINCT unique constraints converge through add, drop, toggle, rename, and unchanged states.", "internal/graphplan#TestNullsNotDistinctScenarioMatrixConvergesOnPostgreSQL", "internal/graphplan#TestConstraintRenamesConvergeOnPostgreSQL")
	case strings.Contains(rel, "constraint/test_index_nulls_not_distinct.py"):
		verified("PostgreSQL 15+ NULLS NOT DISTINCT unique indexes converge through add, drop, toggle, rename, and unchanged states.", "internal/graphplan#TestNullsNotDistinctScenarioMatrixConvergesOnPostgreSQL")
	case strings.Contains(rel, "constraint/test_constraint.py"):
		verified("Primary, unique, check, and exclusion constraints converge through add/drop/rebuild/unchanged, validation in both directions, deferrability, comments, table-owned drops, and primary-key nullability ordering.", "internal/graphplan#TestPrimaryKeyAddModifyAndDropConvergeOnPostgreSQL", "internal/graphplan#TestConstraintAndIndexRebuildConvergeOnPostgreSQL", "internal/graphplan#TestCheckNoInheritNotValidConvergesOnPostgreSQL", "internal/graphplan#TestNotValidConstraintValidationConvergesOnPostgreSQL", "internal/graphplan#TestConstraintsBecomeNotValidConvergeOnPostgreSQL", "internal/graphplan#TestExclusionConstraintCreateAndDropConvergeOnPostgreSQL", "internal/graphplan#TestConstraintDeferrabilityScenarioMatrixConvergesOnPostgreSQL")
	case strings.Contains(rel, "constraint/test_foreign_key.py"):
		verified("Foreign keys converge through add/drop/rebuild/unchanged, pure and combined deferrability changes, validation in both directions, dependency-safe create/drop ordering, NOT VALID, and comments.", "internal/graphplan#TestForeignKeyActionsAndDeferrabilityConvergeOnPostgreSQL", "internal/graphplan#TestConstraintDeferrabilityScenarioMatrixConvergesOnPostgreSQL", "internal/graphplan#TestForeignKeyCycleCreateConvergesOnPostgreSQL", "internal/graphplan#TestForeignKeyDropOrderingScenarioMatrixConvergesOnPostgreSQL", "internal/graphplan#TestNotValidForeignKeyCreateConvergesOnPostgreSQL", "internal/graphplan#TestNotValidConstraintValidationConvergesOnPostgreSQL", "internal/graphplan#TestConstraintsBecomeNotValidConvergeOnPostgreSQL")
	case strings.Contains(rel, "constraint/test_index.py"):
		verified("Ordinary, unique, and partial indexes converge through create/drop/rebuild/unchanged, confirmed rename, comment add/remove/clearing, reused names, table-owned and constraint-owned filtering, and transactional or concurrent lifecycle paths.", "internal/graphplan#TestCreatePlanConvergesOnPostgreSQL", "internal/graphplan#TestConstraintAndIndexRebuildConvergeOnPostgreSQL", "internal/graphplan#TestIndexRenameConvergesOnPostgreSQL", "internal/graphplan#TestIndexRenameAndReusedNameCommentsConvergeOnPostgreSQL", "internal/graphplan#TestCommentTransitionsConvergeOnPostgreSQL", "internal/graphplan#TestApprovedDestructiveDropsConvergeOnPostgreSQL", "internal/graphplan#TestContinuousMaterializedViewAndLocalPartitionIndexReplacementConvergesOnPostgreSQL")
	case strings.Contains(rel, "materialized_view/test_materialized_view_column_dependencies.py"):
		handoff("Column type changes retain the rolling-deployment SQL handoff, which now lists direct, whole-row, transitive materialized-view and index dependencies while excluding unaffected-column readers.", "internal/graphplan#TestMaterializedViewColumnTypeChangeHandoffPreservesDependencyScopeOnPostgreSQL")
	case strings.Contains(rel, "materialized_view/test_materialized_view_index.py") && strings.Contains(name, "recreated_over_retyped_column"):
		handoff("The type-change SQL handoff includes the dependent materialized view and every index that must be preserved or recreated.", "internal/graphplan#TestMaterializedViewColumnTypeChangeHandoffPreservesDependencyScopeOnPostgreSQL")
	case strings.Contains(rel, "materialized_view/test_materialized_view_dependencies.py") && strings.Contains(name, "read_view_definition_changes"):
		handoff("A changed ordinary view requires an explicit materialized-view refresh and verification contract instead of silently choosing locking, refresh mode, or data validation.", "internal/graphplan#TestChangedViewRequiresManualDependentMaterializedViewRefresh")
	case strings.Contains(rel, "materialized_view/test_materialized_view_dependencies.py") && strings.Contains(name, "retyped_column"):
		handoff("The rolling-deployment type-change handoff names the full transitive materialized-view dependency closure and its indexes.", "internal/graphplan#TestMaterializedViewColumnTypeChangeHandoffPreservesDependencyScopeOnPostgreSQL")
	case strings.Contains(rel, "materialized_view/test_materialized_view_dependencies.py"):
		verified("Ordinary-view/materialized-view chains create and drop in dependency order, unchanged chains remain stable, base rebuilds cascade, and ordinary views may safely read materialized views.", "internal/graphplan#TestMaterializedViewDependencyChainsConvergeOnPostgreSQL")
	case strings.Contains(rel, "materialized_view/test_materialized_view_index.py"):
		verified("Materialized-view index create/drop/rename/unchanged/redefinition/unique/comment and concurrent lifecycle behavior converges on real PostgreSQL; rebuilt materialized views restore desired indexes.", "internal/graphplan#TestMaterializedViewIndexLifecycleConvergesOnPostgreSQL", "internal/graphplan#TestMaterializedViewCreateRebuildAndDropConvergeOnPostgreSQL")
	case strings.Contains(rel, "materialized_view/test_materialized_view.py"):
		verified("Materialized-view create/drop/unchanged/rebuild/comments and dependencies on system or extension-owned views converge on real PostgreSQL.", "internal/graphplan#TestMaterializedViewCreateRebuildAndDropConvergeOnPostgreSQL", "internal/graphplan#TestMaterializedViewsOverExternalViewsConvergeOnPostgreSQL")
	case strings.Contains(rel, "table/test_table_column_identity_options.py") && entry.PGMigOutcome == "converges":
		verified("Identity option creation (including descending sequences), add/unchanged/all option transitions/default restoration, generation flips, and serial-to-identity options converge on real PostgreSQL.", "internal/graphplan#TestIdentityAndSerialScenarioMatrixConvergesOnPostgreSQL")
	case strings.Contains(rel, "table/test_table_column_identity_change.py") && entry.PGMigOutcome == "converges":
		verified("Plain/default/serial-to-identity transitions, both generation flips, and confirmed identity removal converge with PostgreSQL-required default/nullability ordering.", "internal/graphplan#TestIdentityAndSerialScenarioMatrixConvergesOnPostgreSQL")
	case strings.Contains(rel, "table/test_table_column_serial.py") && entry.PGMigOutcome == "converges":
		verified("Smallserial/serial/bigserial, primary-key composition, both identity generations, and added identity columns converge on real PostgreSQL.", "internal/graphplan#TestIdentityAndSerialScenarioMatrixConvergesOnPostgreSQL")
	case strings.Contains(rel, "table/test_partitioned_table.py") && (strings.Contains(name, "reparent") || strings.Contains(name, "bound_change") || strings.Contains(name, "strategy_change") || strings.Contains(name, "key_column_change") || strings.Contains(name, "key_expression_change") || strings.Contains(name, "subpartition_key_change") || strings.Contains(name, "recreate_preserves_primary_key")):
		handoff("Onwardpg exposes a clone-verified manual SQL handoff for partition parent, bound, strategy, and key reconfiguration instead of guessing destructive subtree operations; a strategy rewrite explicitly preserves an unchanged primary key.", "internal/graphplan#TestManualPartitionReconfigurationContractConvergesOnPostgreSQL", "internal/graphplan#TestManualPartitionStrategyChangePreservesPrimaryKeyOnPostgreSQL")
	case strings.Contains(rel, "table/test_partitioned_table.py"):
		verified("Range/list/default/hash creation, nested and cross-schema partitions, parent indexes/primary keys, whole-subtree drops, child add/remove, and standalone attach/detach transitions converge on real PostgreSQL.", "internal/graphplan#TestPartitionedTableScenarioMatrixConvergesOnPostgreSQL", "internal/graphplan#TestPartitionAttachAndDetachConvergeOnPostgreSQL", "internal/graphplan#TestPartitionedIndexAndConstraintCreateConvergesOnPostgreSQL")
	case strings.Contains(rel, "table/test_table_column.py") && strings.Contains(name, "_type_") && !strings.Contains(name, "unchanged"):
		handoff("Direct same-name type changes require reviewed expand/contract SQL or a split deployment.", "internal/graphplan#TestTypeMutationProducesEditableExpandContractHandoffOnPostgreSQL")
	case strings.Contains(rel, "table/test_table_column.py"):
		verified("Column physical order, inline/add attributes, add/drop, nullability, defaults, unchanged state, and primary-key-aware DROP NOT NULL ordering converge on real PostgreSQL.", "internal/graphplan#TestTableColumnLifecycleMatrixConvergesOnPostgreSQL")
	case strings.Contains(rel, "general/test_unsupported_type.py") && (strings.Contains(name, "rls_") || strings.Contains(name, "view_return_rule") || strings.Contains(name, "enum_array")):
		entry.Classification = "implemented_unverified"
		entry.Notes = "Onwardpg supports or deliberately filters this shape rather than sharing pgmig's broad refusal; direct evidence remains to be linked."
	}
}
