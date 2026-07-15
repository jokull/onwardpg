// Command stripeinventory deterministically indexes the acceptance corpus from
// the pinned Stripe pg-schema-diff checkout. It is a development/test tool;
// onwardpg production binaries do not import or execute Stripe code.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

const (
	pinnedCommit = "6208f8f3ceccae8ca634055dc47907a6a864cb76"
	pinnedTag    = "v1.0.7"
)

type matrix struct {
	SchemaVersion   int          `json:"schema_version"`
	Reference       reference    `json:"reference"`
	InventoryStatus string       `json:"inventory_status"`
	SourceFiles     []sourceFile `json:"source_files"`
	Cases           []caseEntry  `json:"cases"`
}

type reference struct {
	Repository string `json:"repository"`
	Tag        string `json:"tag"`
	Commit     string `json:"commit"`
	License    string `json:"license"`
}

type sourceFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Cases  int    `json:"cases"`
}

type caseEntry struct {
	ID               string     `json:"id"`
	Capability       string     `json:"capability"`
	Scenario         string     `json:"scenario"`
	StripeTest       string     `json:"stripe_test"`
	SourceLine       int        `json:"source_line"`
	Classification   string     `json:"classification"`
	Dimensions       dimensions `json:"dimensions"`
	OnwardPGTests    []string   `json:"onwardpg_tests,omitempty"`
	DifferentialTest string     `json:"differential_test,omitempty"`
	Notes            string     `json:"notes,omitempty"`
}

type dimensions struct {
	Catalog        string `json:"catalog"`
	Mutation       string `json:"mutation"`
	OnlineStrategy string `json:"online_strategy"`
	Rejection      string `json:"rejection"`
	Workflow       string `json:"workflow"`
}

type familyPolicy struct {
	Capability     string
	Classification string
	Dimensions     dimensions
	Tests          []string
	Notes          string
}

var policies = map[string]familyPolicy{
	"check_constraint":        weaker("constraints.check", []string{"internal/graphplan#TestCheckNoInheritNotValidConvergesOnPostgreSQL", "internal/graphplan#TestNotValidConstraintValidationConvergesOnPostgreSQL"}),
	"column":                  weaker("columns", []string{"internal/graphplan#TestMutationPlanConvergesOnPostgreSQL", "internal/graphplan#TestIdentityGenerationStartAndIncrementConvergeOnPostgreSQL"}),
	"data_packing":            outOfScope("planner.data_packing", "onwardpg preserves declarative column order and does not optimize table layout as a migration-planner side effect"),
	"database_schema_source":  supported("inputs.database_and_ddl", []string{"internal/source#TestLoadGraphDDLEquivalentToLiveDatabase"}),
	"enum":                    weaker("types.enum", []string{"internal/graphplan#TestEnumCreateAndDropConvergeOnPostgreSQL", "internal/graphplan#TestBuildRejectsEnumLabelDropAndReorder"}),
	"extensions":              weaker("extensions", []string{"internal/graphplan#TestExtensionCreateConvergesOnPostgreSQL", "internal/graphplan#TestExtensionSchemaMoveRequiresCompatibilityBridgeOnPostgreSQL"}),
	"foreign_key_constraint":  weaker("constraints.foreign_key", []string{"internal/graphplan#TestForeignKeyActionsAndDeferrabilityConvergeOnPostgreSQL", "internal/graphplan#TestForeignKeyCycleCreateConvergesOnPostgreSQL"}),
	"function":                weaker("routines.function", []string{"internal/graphplan#TestRoutineAndTriggerLifecycleConvergesOnPostgreSQL", "internal/graphplan#TestRoutineViewDependencyOrderingConvergesOnPostgreSQL"}),
	"index":                   weaker("indexes", []string{"internal/graphplan#TestExpressionIndexWithOpClassConvergesOnPostgreSQL", "internal/graphplan#TestConstraintAndIndexRebuildConvergeOnPostgreSQL"}),
	"index_no_concurrent":     weaker("indexes.non_concurrent_option", []string{"internal/graphplan#TestBuildSeparatesConcurrentIndexIntoNonTransactionalBatch"}),
	"local_partition_index":   weaker("indexes.partition_local", []string{"internal/graphplan#TestPartitionedIndexAndConstraintCreateConvergesOnPostgreSQL"}),
	"materialized_view":       weaker("views.materialized", []string{"internal/graphplan#TestMaterializedViewCreateRebuildAndDropConvergeOnPostgreSQL"}),
	"materialized_view_index": weaker("indexes.materialized_view", []string{"internal/source#TestLoadGraphCapturesMaterializedViewIndexes", "internal/graphplan#TestBuildSeparatesConcurrentMaterializedViewRebuildIndex"}),
	"named_schema":            supported("schemas.named", []string{"internal/graphplan#TestCreatePlanConvergesOnPostgreSQL"}),
	"partitioned_index":       weaker("indexes.partitioned", []string{"internal/graphplan#TestPartitionedParentIndexRebuildConvergesOnPostgreSQL"}),
	"partitioned_table":       weaker("tables.partitioned", []string{"internal/graphplan#TestPartitionAttachAndDetachConvergeOnPostgreSQL", "internal/graphplan#TestPartitionedTableCreateConvergesOnPostgreSQL"}),
	"policy": securityWeaker("security.rls_policy", []string{
		"internal/source#TestLoadGraphReportsNarrowlyIgnoredCatalogState",
		"internal/graphplan#TestBuildOrdersPoliciesBeforeRLSAndQuotesRolesAndPrivileges",
		"internal/graphplan#TestRowSecurityPoliciesAndTablePrivilegesConvergeOnPostgreSQL",
	}),
	"privilege": securityWeaker("security.table_privilege", []string{
		"internal/graphplan#TestBuildRequiresAuthorizationAnswersForPolicyAndGrantOptionContractions",
		"internal/graphplan#TestRowSecurityPoliciesAndTablePrivilegesConvergeOnPostgreSQL",
	}),
	"procedure": weaker("routines.procedure", []string{"internal/graphplan#TestRoutineAndTriggerLifecycleConvergesOnPostgreSQL"}),
	"schema":    supported("schemas", []string{"internal/graphplan#TestCommentTransitionsConvergeOnPostgreSQL"}),
	"sequence":  weaker("sequences", []string{"internal/graphplan#TestSequenceParameterUpdateConvergesOnPostgreSQL"}),
	"table":     weaker("tables", []string{"internal/graphplan#TestCreatePlanConvergesOnPostgreSQL", "internal/graphplan#TestApprovedDestructiveDropsConvergeOnPostgreSQL"}),
	"trigger":   weaker("triggers", []string{"internal/graphplan#TestRoutineAndTriggerLifecycleConvergesOnPostgreSQL"}),
	"view":      weaker("views.ordinary", []string{"internal/graphplan#TestViewCreateAndReplaceConvergeOnPostgreSQL"}),
}

func supported(capability string, tests []string) familyPolicy {
	return familyPolicy{Capability: capability, Classification: "supported", Dimensions: dimensions{
		Catalog: "supported", Mutation: "supported", OnlineStrategy: "supported", Rejection: "supported", Workflow: "intentionally_different",
	}, Tests: tests, Notes: "Case-level behavior still requires differential evidence before the matrix can be marked verified."}
}

func weaker(capability string, tests []string) familyPolicy {
	return familyPolicy{Capability: capability, Classification: "weaker", Dimensions: dimensions{
		Catalog: "supported", Mutation: "weaker", OnlineStrategy: "weaker", Rejection: "intentionally_different", Workflow: "intentionally_different",
	}, Tests: tests, Notes: "Conservative family-level classification pending case-by-case differential verification."}
}

func securityWeaker(capability string, tests []string) familyPolicy {
	return familyPolicy{Capability: capability, Classification: "weaker", Dimensions: dimensions{
		Catalog: "supported", Mutation: "supported", OnlineStrategy: "intentionally_different", Rejection: "intentionally_different", Workflow: "intentionally_different",
	}, Tests: tests, Notes: "Typed catalog and mutation support is present; onwardpg adds fingerprint-bound authorization decisions and remains deliberately more conservative about hazards."}
}

func outOfScope(capability, note string) familyPolicy {
	return familyPolicy{Capability: capability, Classification: "out_of_scope", Dimensions: dimensions{
		Catalog: "out_of_scope", Mutation: "out_of_scope", OnlineStrategy: "out_of_scope", Rejection: "intentionally_different", Workflow: "out_of_scope",
	}, Notes: note}
}

func main() {
	source := flag.String("source", ".stripe-pg-schema-diff", "path to the pinned Stripe checkout")
	output := flag.String("output", "parity/stripe-pg-schema-diff-v1.0.7.json", "output matrix path")
	flag.Parse()

	if err := run(*source, *output); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(source, output string) error {
	commit, err := exec.Command("git", "-C", source, "rev-parse", "HEAD").Output()
	if err != nil {
		return fmt.Errorf("read Stripe checkout commit: %w", err)
	}
	if actual := strings.TrimSpace(string(commit)); actual != pinnedCommit {
		return fmt.Errorf("stripe checkout is %s; expected %s", actual, pinnedCommit)
	}

	root := filepath.Join(source, "internal", "migration_acceptance_tests")
	files, err := filepath.Glob(filepath.Join(root, "*_cases_test.go"))
	if err != nil {
		return err
	}
	sort.Strings(files)

	result := matrix{
		SchemaVersion:   1,
		Reference:       reference{Repository: "https://github.com/stripe/pg-schema-diff", Tag: pinnedTag, Commit: pinnedCommit, License: "MIT"},
		InventoryStatus: "classified_unverified",
	}
	seen := make(map[string]int)
	for _, path := range files {
		entries, fileRecord, err := inspectFile(root, path)
		if err != nil {
			return err
		}
		for i := range entries {
			base := entries[i].ID
			seen[base]++
			if seen[base] > 1 {
				entries[i].ID = fmt.Sprintf("%s.%d", base, seen[base])
			}
		}
		result.SourceFiles = append(result.SourceFiles, fileRecord)
		result.Cases = append(result.Cases, entries...)
	}
	if len(result.Cases) == 0 {
		return fmt.Errorf("no Stripe acceptance cases found in %s", root)
	}

	encoded, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	if err := os.MkdirAll(filepath.Dir(output), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(output, encoded, 0o644); err != nil {
		return err
	}
	return nil
}

func inspectFile(root, path string) ([]caseEntry, sourceFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, sourceFile{}, err
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, data, 0)
	if err != nil {
		return nil, sourceFile{}, err
	}
	rel, err := filepath.Rel(filepath.Dir(root), path)
	if err != nil {
		return nil, sourceFile{}, err
	}
	rel = filepath.ToSlash(rel)
	family := strings.TrimSuffix(filepath.Base(path), "_cases_test.go")
	policy, ok := policies[family]
	if !ok {
		return nil, sourceFile{}, fmt.Errorf("acceptance family %q has no classification policy", family)
	}

	testName := ""
	for _, declaration := range file.Decls {
		fn, ok := declaration.(*ast.FuncDecl)
		if ok && fn.Body != nil && strings.HasPrefix(fn.Name.Name, "Test") {
			if testName != "" {
				return nil, sourceFile{}, fmt.Errorf("%s has multiple acceptance Test functions", rel)
			}
			testName = fn.Name.Name
		}
	}
	if testName == "" {
		return nil, sourceFile{}, fmt.Errorf("%s has no acceptance Test function", rel)
	}

	var entries []caseEntry
	ast.Inspect(file, func(node ast.Node) bool {
		pair, ok := node.(*ast.KeyValueExpr)
		if !ok {
			return true
		}
		key, ok := pair.Key.(*ast.Ident)
		if !ok || key.Name != "name" {
			return true
		}
		literal, ok := pair.Value.(*ast.BasicLit)
		if !ok || literal.Kind != token.STRING {
			return true
		}
		name, err := strconv.Unquote(literal.Value)
		if err != nil {
			return true
		}
		entry := caseEntry{
			ID:             family + "." + slug(name),
			Capability:     policy.Capability,
			Scenario:       name,
			StripeTest:     rel + "#" + testName + "/" + name,
			SourceLine:     fset.Position(pair.Pos()).Line,
			Classification: policy.Classification,
			Dimensions:     policy.Dimensions,
			OnwardPGTests:  append([]string(nil), policy.Tests...),
			Notes:          policy.Notes,
		}
		applyCaseOverrides(&entry, family, name)
		entries = append(entries, entry)
		return true
	})
	sort.Slice(entries, func(i, j int) bool { return entries[i].SourceLine < entries[j].SourceLine })
	digest := sha256.Sum256(data)
	return entries, sourceFile{Path: rel, SHA256: hex.EncodeToString(digest[:]), Cases: len(entries)}, nil
}

func applyCaseOverrides(entry *caseEntry, family, name string) {
	switch {
	case family == "table" && name == "Add NOT NULL column without default":
		entry.OnwardPGTests = []string{
			"internal/graphplan#TestBuildStagesNewRequiredColumnWithoutDefault",
			"internal/graphplan#TestRequiredColumnStagingConvergesOnPostgreSQL",
		}
		entry.Notes = "onwardpg intentionally stages this as nullable expand, editable dual-write/backfill work, and NOT NULL contract instead of attempting one direct statement."
	case family == "table" && name == "Add NOT NULL column with constant default avoids backfill hazard":
		entry.OnwardPGTests = []string{"internal/graphplan#TestBuildAddsRequiredColumnWithDefaultDirectly"}
		entry.Notes = "A retained default supplies old writers and existing rows, so onwardpg emits the direct additive column DDL with explicit lock/rewrite hazards."
	case family == "column" && name == "Add one column and change ordering":
		entry.OnwardPGTests = []string{
			"internal/graphplan#TestPlanRejectsUnreachablePhysicalColumnOrder",
			"cmd/onwardpg#TestDraftReportsUnreachableColumnOrderBeforeWritingBundleOnPostgreSQL",
		}
		entry.Notes = "PostgreSQL can append a column but cannot insert it before retained physical columns. onwardpg blocks that declarative transition with a typed column_physical_order finding before writing or replacing a bundle instead of accepting a plan that leaves a residual diff."
	case family == "index" && name == "Alter index columns (index replacement and prioritized builds)":
		entry.Classification = "supported"
		entry.Dimensions.Mutation = "supported"
		entry.Dimensions.OnlineStrategy = "supported"
		entry.OnwardPGTests = []string{
			"internal/graphplan#TestBuildContinuouslyReplacesSameNameIndex",
			"internal/differential#TestPinnedStripeAndOnwardPGContinuousIndexReplacement",
		}
		entry.DifferentialTest = "internal/differential#TestPinnedStripeAndOnwardPGContinuousIndexReplacement"
		entry.Notes = "Both planners preserve a usable old index, build the desired same-name index concurrently, then remove the temporary old index concurrently. SQL identifiers differ intentionally."
	case family == "index" && name == "Alter primary key columns (name stays same)":
		entry.Classification = "supported"
		entry.Dimensions.Mutation = "supported"
		entry.Dimensions.OnlineStrategy = "supported"
		entry.OnwardPGTests = []string{
			"internal/graphplan#TestContinuousPrimaryAndUniqueConstraintReplacementConvergesOnPostgreSQL",
			"internal/differential#TestPinnedStripePrimaryConstraintCaseRequiresOnwardTypeBridgeIntent",
		}
		entry.DifferentialTest = "internal/differential#TestPinnedStripePrimaryConstraintCaseRequiresOnwardTypeBridgeIntent"
		entry.Classification = "weaker"
		entry.Dimensions.Mutation = "weaker"
		entry.Dimensions.OnlineStrategy = "intentionally_different"
		entry.Notes = "Stripe converges with a direct cast and replacement constraint index. onwardpg can plan the index transition but requires reviewed expand/contract type-bridge SQL (or another deployment) instead of treating the cast as rolling-deploy compatibility."
	case family == "materialized_view_index" && name == "Change index columns on materialized view":
		entry.Classification = "supported"
		entry.Dimensions.Mutation = "supported"
		entry.Dimensions.OnlineStrategy = "supported"
		entry.OnwardPGTests = []string{
			"internal/graphplan#TestContinuousMaterializedViewAndLocalPartitionIndexReplacementConvergesOnPostgreSQL",
			"internal/differential#TestPinnedStripeAndOnwardPGContinuousMaterializedViewAndLocalIndexReplacement",
		}
		entry.DifferentialTest = "internal/differential#TestPinnedStripeAndOnwardPGContinuousMaterializedViewAndLocalIndexReplacement"
		entry.Notes = "Both planners continuously replace the same-name materialized-view index and converge without a residual diff."
	case family == "local_partition_index" && name == "Change an index columns (with conflicting schemas)":
		entry.Classification = "supported"
		entry.Dimensions.Mutation = "supported"
		entry.Dimensions.OnlineStrategy = "supported"
		entry.OnwardPGTests = []string{
			"internal/graphplan#TestContinuousMaterializedViewAndLocalPartitionIndexReplacementConvergesOnPostgreSQL",
			"internal/differential#TestPinnedStripeAndOnwardPGContinuousMaterializedViewAndLocalIndexReplacement",
		}
		entry.DifferentialTest = "internal/differential#TestPinnedStripeAndOnwardPGContinuousMaterializedViewAndLocalIndexReplacement"
		entry.Notes = "Both planners continuously replace same-named independent local partition indexes in distinct schemas using collision-safe temporary identities."
	case family == "partitioned_index" && name == "Change an index column ordering":
		entry.Classification = "supported"
		entry.Dimensions.Mutation = "supported"
		entry.Dimensions.OnlineStrategy = "supported"
		entry.OnwardPGTests = []string{
			"internal/graphplan#TestContinuousPartitionedParentIndexReplacementConvergesOnPostgreSQL",
			"internal/differential#TestPinnedStripeAndOnwardPGContinuousPartitionedParentIndexReplacement",
		}
		entry.DifferentialTest = "internal/differential#TestPinnedStripeAndOnwardPGContinuousPartitionedParentIndexReplacement"
		entry.Notes = "Both planners retain the valid old hierarchy, create an ONLY parent shell, build leaf indexes concurrently, attach them, then retire the old hierarchy without a committed availability gap."
	case family == "partitioned_index" && name == "Attach an unnattached, invalid index":
		entry.Classification = "supported"
		entry.Dimensions.Mutation = "supported"
		entry.Dimensions.OnlineStrategy = "supported"
		entry.OnwardPGTests = []string{
			"internal/graphplan#TestAttachExistingPartitionIndexConvergesOnPostgreSQL",
			"internal/differential#TestPinnedStripeAndOnwardPGAttachExistingPartitionIndex",
		}
		entry.DifferentialTest = "internal/differential#TestPinnedStripeAndOnwardPGAttachExistingPartitionIndex"
		entry.Notes = "Both planners attach an already-built structurally matching local index to the incomplete partitioned parent without rebuilding or dropping either index. Detach, reparent, and structural mutation remain explicit rejections."
	case family == "partitioned_index" && (name == "Add primary key to partition using existing index" || name == "Add unique constraint to partition using existing index"):
		entry.Classification = "supported"
		entry.Dimensions.Mutation = "supported"
		entry.Dimensions.OnlineStrategy = "supported"
		entry.OnwardPGTests = []string{
			"internal/graphplan#TestAttachExistingPartitionConstraintIndexesConvergeOnPostgreSQL",
			"internal/differential#TestPinnedStripeAndOnwardPGAttachExistingPartitionConstraintIndexes",
		}
		entry.DifferentialTest = "internal/differential#TestPinnedStripeAndOnwardPGAttachExistingPartitionConstraintIndexes"
		entry.Notes = "Both planners let a new local primary/unique constraint claim the prebuilt matching unique index, then attach the constraint-owned index to its parent in dependency order. Identity or structural mismatches reject."
	case family == "sequence" && strings.HasPrefix(name, "Alter ownership"):
		entry.Classification = "supported"
		entry.Dimensions.Mutation = "supported"
		entry.OnwardPGTests = []string{
			"internal/graphplan#TestSequenceOwnedByTransitionsConvergeOnPostgreSQL",
			"internal/differential#TestPinnedStripeAndOnwardPGSequenceOwnershipAndIdentityOptions",
		}
		entry.DifferentialTest = "internal/differential#TestPinnedStripeAndOnwardPGSequenceOwnershipAndIdentityOptions"
		entry.Notes = "OWNED BY NONE, none-to-column, column-to-none, and column-to-column transitions are typed, dependency-ordered, and converge."
	case family == "column" && strings.Contains(strings.ToLower(name), "identity"):
		entry.Classification = "supported"
		entry.Dimensions.Mutation = "supported"
		entry.OnwardPGTests = []string{
			"internal/graphplan#TestIdentityAddAllOptionsAndConfirmedDropConvergeOnPostgreSQL",
			"internal/differential#TestPinnedStripeAndOnwardPGSequenceOwnershipAndIdentityOptions",
		}
		entry.DifferentialTest = "internal/differential#TestPinnedStripeAndOnwardPGSequenceOwnershipAndIdentityOptions"
		entry.Notes = "Identity create/add, generation and sequence options, and fingerprint-confirmed destructive removal are typed and converge."
	case family == "policy" && (name == "Alter policy using" || name == "Alter policy check"):
		entry.OnwardPGTests = append(entry.OnwardPGTests, "internal/differential#TestPinnedStripeAndOnwardPGRowSecurityPoliciesAndPrivileges")
		entry.DifferentialTest = "internal/differential#TestPinnedStripeAndOnwardPGRowSecurityPoliciesAndPrivileges"
		entry.Notes = "The differential corpus proves typed expression alteration, authorization hazards, timeout guidance, idempotence, and final convergence; onwardpg additionally requires a fingerprint-bound policy decision."
	case family == "privilege" && (name == "Revoke privilege from role" || name == "Remove GRANT OPTION (recreates privilege)"):
		entry.OnwardPGTests = append(entry.OnwardPGTests, "internal/differential#TestPinnedStripeAndOnwardPGRowSecurityPoliciesAndPrivileges")
		entry.DifferentialTest = "internal/differential#TestPinnedStripeAndOnwardPGRowSecurityPoliciesAndPrivileges"
		entry.Notes = "The differential corpus proves revoke and grant-option contraction, hazards, timeout guidance, idempotence, and final convergence; onwardpg additionally requires fingerprint-bound destructive intent."
	}
}

func slug(value string) string {
	var out strings.Builder
	separator := false
	for _, r := range strings.ToLower(value) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			if separator && out.Len() > 0 {
				out.WriteByte('_')
			}
			out.WriteRune(r)
			separator = false
			continue
		}
		separator = true
	}
	result := strings.Trim(out.String(), "_")
	if len(result) > 80 {
		digest := sha256.Sum256([]byte(value))
		result = strings.TrimRight(result[:64], "_") + "_" + hex.EncodeToString(digest[:6])
	}
	return result
}
