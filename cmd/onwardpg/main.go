package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jokull/onwardpg/internal/bundle"
	"github.com/jokull/onwardpg/internal/draftflow"
	"github.com/jokull/onwardpg/internal/driftcheck"
	"github.com/jokull/onwardpg/internal/graphplan"
	"github.com/jokull/onwardpg/internal/history"
	"github.com/jokull/onwardpg/internal/historyinit"
	"github.com/jokull/onwardpg/internal/protocol"
	"github.com/jokull/onwardpg/internal/semantichint"
	"github.com/jokull/onwardpg/internal/source"
	"github.com/jokull/onwardpg/internal/verify"
	"github.com/jokull/onwardpg/internal/workspace"
)

var buildVersion = "dev"

func main() { os.Exit(run()) }

func run() int {
	if len(os.Args) < 2 {
		return writeError("invalid_invocation", errors.New("usage: onwardpg <init|dev|draft|verify|drift|plan|config|version>"))
	}
	switch os.Args[1] {
	case "version", "--version":
		_ = json.NewEncoder(os.Stdout).Encode(struct {
			Version string `json:"version"`
		}{Version: buildVersion})
		return 0
	case "init":
		return runInit(os.Args[2:])
	case "plan":
		return runPlan(os.Args[2:])
	case "dev":
		return runDev(os.Args[2:])
	case "draft":
		return runDraft(os.Args[2:])
	case "verify":
		return runVerify(os.Args[2:])
	case "drift":
		return runDrift(os.Args[2:])
	case "config":
		return runConfig(os.Args[2:])
	default:
		return writeError("invalid_invocation", fmt.Errorf("unknown command %q", os.Args[1]))
	}
}

func runInit(arguments []string) int {
	return runInitAt(arguments, ".")
}

func runInitAt(arguments []string, start string) int {
	return runHistoryAt(append([]string{"init"}, arguments...), start)
}

func runVerify(arguments []string) int { return runVerifyAt(arguments, ".") }

func runVerifyAt(arguments []string, start string) int {
	return runBundleAt(append([]string{"verify"}, arguments...), start)
}

func runDrift(arguments []string) int { return runDriftAt(arguments, ".") }

func runDriftAt(arguments []string, start string) int {
	if len(arguments) == 0 || arguments[0] != "check" {
		return writeError("invalid_invocation", errors.New("usage: onwardpg drift check --target NAME --database URL"))
	}
	flags := flag.NewFlagSet("drift check", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	targetName := flags.String("target", "", "configured database target name")
	databaseURL := flags.String("database", "", "live PostgreSQL URL inspected read-only")
	configName := flags.String("config", ".onwardpg.toml", "repository configuration path")
	var ignores stringsFlag
	flags.Var(&ignores, "ignore", "validated catalog selector to exclude")
	if err := flags.Parse(arguments[1:]); err != nil {
		return writeError("invalid_invocation", err)
	}
	if *targetName == "" || *databaseURL == "" {
		return writeError("invalid_invocation", errors.New("drift check requires --target and --database"))
	}
	configPath := *configName
	if !filepath.IsAbs(configPath) {
		configPath = filepath.Join(start, configPath)
	}
	configPath, err := filepath.Abs(configPath)
	if err != nil {
		return writeError("invalid_config", err)
	}
	config, err := workspace.Load(configPath)
	if err != nil {
		return writeError("invalid_config", err)
	}
	target, err := config.Target(*targetName)
	if err != nil {
		return writeError("invalid_config", err)
	}
	scratchEnv := target.ScratchEnv()
	scratchURL := os.Getenv(scratchEnv)
	if scratchURL == "" {
		return writeError("source_error", fmt.Errorf("environment variable %s is required", scratchEnv))
	}
	root := filepath.Dir(configPath)
	chain, err := history.Load(root, config.BundleRoot, *targetName)
	if err != nil {
		return writeError("invalid_history", err)
	}
	if len(chain.Entries) == 0 {
		return writeError("invalid_history", errors.New("target history is empty; run onwardpg init first"))
	}
	replay, err := chain.Replay()
	if err != nil {
		return writeError("invalid_history", err)
	}
	ctx := context.Background()
	expected, err := source.LoadDDLGraphForComparison(ctx, replay.DDL, replay.Provenance, scratchURL, ignores)
	if err != nil {
		return writeError("source_error", fmt.Errorf("replay expected history: %w", err))
	}
	actual, err := source.LoadGraphForComparison(ctx, source.Parse(*databaseURL), "", ignores)
	if err != nil {
		return writeError("source_error", fmt.Errorf("inspect live catalog: %w", err))
	}
	if err := source.ValidateIgnoreSelectors(ignores, expected, actual); err != nil {
		return writeError("invalid_ignore", err)
	}
	report, err := driftcheck.Compare(*targetName, chain.HeadDigest, expected, actual)
	if err != nil {
		return writeError("drift_error", err)
	}
	_ = json.NewEncoder(os.Stdout).Encode(report)
	if report.Outcome == "drift_free" {
		return 0
	}
	return 4
}

func runHistoryAt(arguments []string, start string) int {
	if len(arguments) == 0 || arguments[0] != "init" {
		return writeError("invalid_invocation", errors.New("usage: onwardpg init --target NAME [--bundle baseline]"))
	}
	flags := flag.NewFlagSet("init", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	targetName := flags.String("target", "", "configured database target name")
	bundleID := flags.String("bundle", "baseline", "root history bundle identifier")
	configName := flags.String("config", ".onwardpg.toml", "repository configuration path")
	concurrentIndexes := flags.Bool("concurrent-indexes", false, "create standalone indexes concurrently")
	var ignores stringsFlag
	flags.Var(&ignores, "ignore", "validated catalog selector to exclude")
	if err := flags.Parse(arguments[1:]); err != nil {
		return writeError("invalid_invocation", err)
	}
	if *targetName == "" {
		return writeError("invalid_invocation", errors.New("init requires --target"))
	}
	configPath := *configName
	if !filepath.IsAbs(configPath) {
		configPath = filepath.Join(start, configPath)
	}
	configPath, err := filepath.Abs(configPath)
	if err != nil {
		return writeError("invalid_config", err)
	}
	config, err := workspace.Load(configPath)
	if err != nil {
		return writeError("invalid_config", err)
	}
	target, err := config.Target(*targetName)
	if err != nil {
		return writeError("invalid_config", err)
	}
	adminEnv := target.ScratchEnv()
	adminURL := os.Getenv(adminEnv)
	if adminURL == "" {
		return writeError("source_error", fmt.Errorf("environment variable %s is required", adminEnv))
	}
	selectors := sortedUniqueStrings(ignores)
	report, err := historyinit.Run(context.Background(), historyinit.Input{
		Root:           filepath.Dir(configPath),
		Config:         config,
		TargetName:     *targetName,
		Target:         target,
		AdminURL:       adminURL,
		BundleID:       *bundleID,
		BuildVersion:   buildVersion,
		Ignores:        selectors,
		PlannerOptions: graphplan.Options{ConcurrentIndexes: *concurrentIndexes},
	})
	if err != nil {
		return writeError("history_init_error", err)
	}
	_ = json.NewEncoder(os.Stdout).Encode(report)
	switch report.Outcome {
	case "initialized":
		return 0
	case string(protocol.NeedsInput):
		return 2
	case string(protocol.Unsupported):
		return 3
	case "blocked":
		return 4
	default:
		return 1
	}
}

func runDev(arguments []string) int { return runDevAt(arguments, ".") }

func runDevAt(arguments []string, start string) int {
	if len(arguments) == 0 || arguments[0] != "plan" {
		return writeError("invalid_invocation", errors.New("usage: onwardpg dev plan --target NAME [--hint JSON] [--hints-file FILE] [--output text|json]"))
	}
	flags := flag.NewFlagSet("dev plan", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	targetName := flags.String("target", "", "configured database target name")
	configName := flags.String("config", ".onwardpg.toml", "repository configuration path")
	var inlineHints stringsFlag
	flags.Var(&inlineHints, "hint", "semantic JSON hint; repeat for multiple decisions")
	hintsFile := flags.String("hints-file", "", "JSON array of semantic hints")
	output := flags.String("output", "json", "output format: text or json")
	concurrentIndexes := flags.Bool("concurrent-indexes", false, "create standalone indexes concurrently")
	ifNotExists := flags.Bool("if-not-exists", false, "emit IF NOT EXISTS for schema and table creation")
	ifExists := flags.Bool("if-exists", false, "emit IF EXISTS for schema and table drops")
	cascadeDrops := flags.Bool("cascade-drops", false, "emit CASCADE for schema and table drops")
	var schemaQualifier optionalString
	flags.Var(&schemaQualifier, "schema-qualifier", "scope to one schema and render names using this qualifier")
	var ignores stringsFlag
	flags.Var(&ignores, "ignore", "validated catalog selector to exclude")
	if err := flags.Parse(arguments[1:]); err != nil {
		return writeError("invalid_invocation", err)
	}
	if *targetName == "" {
		return writeError("invalid_invocation", errors.New("dev plan requires --target"))
	}
	if *output != "json" && *output != "text" {
		return writeError("invalid_invocation", errors.New("dev plan --output must be text or json"))
	}
	configPath := *configName
	if !filepath.IsAbs(configPath) {
		configPath = filepath.Join(start, configPath)
	}
	configPath, err := filepath.Abs(configPath)
	if err != nil {
		return writeError("invalid_config", err)
	}
	config, err := workspace.Load(configPath)
	if err != nil {
		return writeError("invalid_config", err)
	}
	target, err := config.Target(*targetName)
	if err != nil {
		return writeError("invalid_config", err)
	}
	devURL := os.Getenv(target.DevDatabaseEnv)
	if devURL == "" {
		return writeError("source_error", fmt.Errorf("environment variable %s is required", target.DevDatabaseEnv))
	}
	scratchEnv := target.ScratchEnv()
	scratchURL := os.Getenv(scratchEnv)
	if scratchURL == "" {
		return writeError("source_error", fmt.Errorf("environment variable %s is required", scratchEnv))
	}
	hints, err := readHints(inlineHints, *hintsFile)
	if err != nil {
		return writeError("invalid_hints", err)
	}
	ctx := context.Background()
	compiled, err := workspace.CompileDDL(ctx, filepath.Dir(configPath), *targetName, target)
	if err != nil {
		return writeError("source_error", err)
	}
	current, err := source.LoadGraphForComparison(ctx, source.Parse(devURL), "", ignores)
	if err != nil {
		return writeError("source_error", err)
	}
	desired, err := source.LoadDDLGraphForComparison(ctx, compiled.DDL, compiled.Provenance, scratchURL, ignores)
	if err != nil {
		return writeError("source_error", err)
	}
	if err := source.ValidateIgnoreSelectors(ignores, current, desired); err != nil {
		return writeError("invalid_ignore", err)
	}
	options := graphplan.Options{
		ConcurrentIndexes: *concurrentIndexes, IfNotExists: *ifNotExists,
		IfExists: *ifExists, CascadeDrops: *cascadeDrops,
	}
	if schemaQualifier.set {
		options.SchemaQualifier = &schemaQualifier.value
	}
	resolution, err := semantichint.Resolve(current, desired, hints, options)
	if err != nil {
		return writeError("planning_error", err)
	}
	result := resolution.Result
	if result.Status == protocol.NeedsInput {
		decisions, decisionErr := semantichint.Decisions(result.Questions, current, desired)
		if decisionErr != nil {
			return writeError("planning_error", decisionErr)
		}
		var outputErr error
		if *output == "text" {
			outputErr = writeDecisionsText(os.Stdout, "dev plan", decisions)
		} else {
			outputErr = writeDecisionEnvelope(os.Stdout, "onwardpg/dev-plan/2", decisions)
		}
		if outputErr != nil {
			return writeError("output_error", outputErr)
		}
	} else if *output == "text" {
		if result.Status == protocol.Unsupported {
			_, _ = fmt.Fprintln(os.Stdout, "unsupported")
			for _, reason := range result.Unsupported {
				_, _ = fmt.Fprintln(os.Stdout, "  "+reason)
			}
		} else {
			_, _ = fmt.Fprintln(os.Stdout, protocol.RenderSQL(result, ""))
		}
	} else {
		_ = json.NewEncoder(os.Stdout).Encode(result)
	}
	return resultExitCode(result.Status)
}

func runDraft(arguments []string) int { return runDraftAt(arguments, ".") }

func runDraftAt(arguments []string, start string) int {
	flags := flag.NewFlagSet("draft", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	targetName := flags.String("target", "", "configured database target name")
	bundleID := flags.String("bundle", "", "stable logical feature bundle identifier")
	configName := flags.String("config", ".onwardpg.toml", "repository configuration path")
	var inlineHints stringsFlag
	flags.Var(&inlineHints, "hint", "semantic JSON hint; repeat for multiple decisions")
	hintsFile := flags.String("hints-file", "", "JSON array of semantic hints")
	output := flags.String("output", "json", "output format: text or json")
	purpose := flags.String("purpose", "feature", "feature, repair, or contract")
	concurrentIndexes := flags.Bool("concurrent-indexes", false, "create standalone indexes concurrently")
	ifNotExists := flags.Bool("if-not-exists", false, "emit IF NOT EXISTS for schema and table creation")
	ifExists := flags.Bool("if-exists", false, "emit IF EXISTS for schema and table drops")
	cascadeDrops := flags.Bool("cascade-drops", false, "emit CASCADE for schema and table drops")
	var schemaQualifier optionalString
	flags.Var(&schemaQualifier, "schema-qualifier", "scope to one schema and render names using this qualifier")
	var ignores stringsFlag
	flags.Var(&ignores, "ignore", "validated catalog selector to exclude")
	if err := flags.Parse(arguments); err != nil {
		return writeError("invalid_invocation", err)
	}
	if *targetName == "" || *bundleID == "" {
		return writeError("invalid_invocation", errors.New("draft requires --target and --bundle"))
	}
	if *output != "json" && *output != "text" {
		return writeError("invalid_invocation", errors.New("draft --output must be text or json"))
	}
	configPath := *configName
	if !filepath.IsAbs(configPath) {
		configPath = filepath.Join(start, configPath)
	}
	configPath, err := filepath.Abs(configPath)
	if err != nil {
		return writeError("invalid_config", err)
	}
	config, err := workspace.Load(configPath)
	if err != nil {
		return writeError("invalid_config", err)
	}
	target, err := config.Target(*targetName)
	if err != nil {
		return writeError("invalid_config", err)
	}
	adminEnv := target.ScratchEnv()
	adminURL := os.Getenv(adminEnv)
	if adminURL == "" {
		return writeError("source_error", fmt.Errorf("environment variable %s is required", adminEnv))
	}
	hints, err := readHints(inlineHints, *hintsFile)
	if err != nil {
		return writeError("invalid_hints", err)
	}
	options := graphplan.Options{
		ConcurrentIndexes: *concurrentIndexes,
		IfNotExists:       *ifNotExists,
		IfExists:          *ifExists,
		CascadeDrops:      *cascadeDrops,
	}
	if schemaQualifier.set {
		options.SchemaQualifier = &schemaQualifier.value
	}
	report, err := draftflow.Run(context.Background(), draftflow.Input{
		Root:           filepath.Dir(configPath),
		Config:         config,
		TargetName:     *targetName,
		Target:         target,
		AdminURL:       adminURL,
		BundleID:       *bundleID,
		BuildVersion:   buildVersion,
		Purpose:        *purpose,
		Hints:          hints,
		HintsGiven:     len(inlineHints) > 0 || *hintsFile != "",
		Ignores:        sortedUniqueStrings(ignores),
		PlannerOptions: options,
	})
	if err != nil {
		return writeError("draft_error", err)
	}
	if err := writeDraftReport(os.Stdout, report, *output); err != nil {
		return writeError("output_error", err)
	}
	switch report.Outcome {
	case string(protocol.Planned):
		return 0
	case string(protocol.NeedsInput), string(protocol.NeedsSQLEdits):
		return 2
	case string(protocol.Unsupported):
		return 3
	case "blocked":
		return 4
	default:
		return 1
	}
}

func runBundleAt(arguments []string, start string) int {
	if len(arguments) == 0 || arguments[0] != "verify" {
		return writeError("invalid_invocation", errors.New("usage: onwardpg verify --target NAME --bundle ID [--through PHASE]"))
	}
	flags := flag.NewFlagSet("verify", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	targetName := flags.String("target", "", "configured database target name")
	bundleID := flags.String("bundle", "", "bundle to verify")
	through := flags.String("through", "contract", "last phase to execute: expand, migrate, or contract")
	check := flags.Bool("check", false, "read-only verification; reject unreceipted edits")
	configName := flags.String("config", ".onwardpg.toml", "repository configuration path")
	if err := flags.Parse(arguments[1:]); err != nil {
		return writeError("invalid_invocation", err)
	}
	if *targetName == "" || *bundleID == "" {
		return writeError("invalid_invocation", errors.New("verify requires --target and --bundle"))
	}
	configPath := *configName
	if !filepath.IsAbs(configPath) {
		configPath = filepath.Join(start, configPath)
	}
	configPath, err := filepath.Abs(configPath)
	if err != nil {
		return writeError("invalid_config", err)
	}
	config, err := workspace.Load(configPath)
	if err != nil {
		return writeError("invalid_config", err)
	}
	target, err := config.Target(*targetName)
	if err != nil {
		return writeError("invalid_config", err)
	}
	adminEnv := target.ScratchEnv()
	adminURL := os.Getenv(adminEnv)
	if adminURL == "" {
		return writeError("source_error", fmt.Errorf("environment variable %s is required", adminEnv))
	}
	root := filepath.Dir(configPath)
	chain, err := history.Load(root, config.BundleRoot, *targetName)
	var edited *history.Entry
	if err != nil {
		strictErr := err
		base, selected, prepareErr := history.LoadEditedDraft(root, config.BundleRoot, *targetName, *bundleID)
		if prepareErr != nil {
			code, remediation := "invalid_history", "restore the receipted history chain, then rerun onwardpg verify --check"
			if strings.Contains(prepareErr.Error(), "ONWARDPG TODO") {
				code = "unresolved_sql_todo"
				remediation = "replace every ONWARDPG TODO in the reported phase file, then run onwardpg verify --target " + *targetName + " --bundle " + *bundleID
			}
			return writeVerifyFinding(*targetName, *bundleID, "", *through, "blocked", code,
				fmt.Sprintf("strict history validation failed: %v; editable bundle preparation failed: %v", strictErr, prepareErr), remediation)
		}
		if selected == nil {
			return writeVerifyFinding(*targetName, *bundleID, base.HeadDigest, *through, "blocked", "invalid_history",
				strictErr.Error(), "restore the receipted history chain, then rerun onwardpg verify --check")
		}
		if selected.Artifact.Manifest.History.ParentDigest != base.HeadDigest {
			return writeVerifyFinding(*targetName, *bundleID, base.HeadDigest, *through, "stale", "stale_history_parent",
				fmt.Sprintf("edited bundle parent %s is stale; current history head is %s", selected.Artifact.Manifest.History.ParentDigest, base.HeadDigest),
				"run onwardpg draft --target "+*targetName+" --bundle "+*bundleID+" to restack the selected bundle")
		}
		if *check {
			return writeVerifyFinding(*targetName, *bundleID, base.HeadDigest, *through, "blocked", "unreceipted_sql_edits",
				"the selected bundle contains valid SQL edits that have not been clone-verified and receipted",
				"run onwardpg verify --target "+*targetName+" --bundle "+*bundleID+"; use --check only after it succeeds")
		}
		edited = selected
		chain = base
		chain.Entries = append(chain.Entries, *selected)
		chain.HeadDigest = selected.Artifact.Manifest.History.EntryDigest
	}
	if *check && (len(chain.Entries) == 0 || chain.Entries[len(chain.Entries)-1].Artifact.Manifest.BundleID != *bundleID) {
		head := ""
		if len(chain.Entries) > 0 {
			head = chain.Entries[len(chain.Entries)-1].Artifact.Manifest.BundleID
		}
		_ = json.NewEncoder(os.Stdout).Encode(verify.Report{
			ProtocolVersion: verify.Version,
			Outcome:         "blocked",
			Target:          *targetName,
			BundleID:        *bundleID,
			HistoryHead:     chain.HeadDigest,
			ThroughPhase:    *through,
			Findings: []verify.Finding{{
				Code:        "bundle_not_history_head",
				Message:     fmt.Sprintf("bundle %q is not the target history head; current head is %q", *bundleID, head),
				Remediation: "run onwardpg verify --check for the selected head bundle, or draft the feature on top of the current history head",
			}},
		})
		return 4
	}
	prefix, err := chain.Through(*bundleID)
	if err != nil {
		return writeError("invalid_history", err)
	}
	manifest := prefix.Entries[len(prefix.Entries)-1].Artifact.Manifest
	options := graphplan.Options{
		ConcurrentIndexes: manifest.Planner.Options.ConcurrentIndexes,
		IfNotExists:       manifest.Planner.Options.IfNotExists,
		IfExists:          manifest.Planner.Options.IfExists,
		CascadeDrops:      manifest.Planner.Options.CascadeDrops,
		SchemaQualifier:   manifest.Planner.Options.SchemaQualifier,
	}
	ctx := context.Background()
	compiled, err := workspace.CompileDDL(ctx, root, *targetName, target)
	if err != nil {
		return writeError("source_error", fmt.Errorf("compile current desired schema: %w", err))
	}
	working, err := source.LoadDDLGraphForComparison(ctx, compiled.DDL, compiled.Provenance, adminURL, manifest.Planner.IgnoreSelectors)
	if err != nil {
		return writeError("source_error", fmt.Errorf("materialize current desired schema: %w", err))
	}
	workingFingerprint, err := working.Fingerprint()
	if err != nil {
		return writeError("source_error", fmt.Errorf("fingerprint current desired schema: %w", err))
	}
	if workingFingerprint != manifest.DesiredSource.Fingerprint {
		_ = json.NewEncoder(os.Stdout).Encode(verify.Report{
			ProtocolVersion:    verify.Version,
			Outcome:            "stale",
			Target:             *targetName,
			BundleID:           *bundleID,
			HistoryHead:        prefix.HeadDigest,
			ThroughPhase:       *through,
			DesiredFingerprint: manifest.DesiredSource.Fingerprint,
			WorkingFingerprint: workingFingerprint,
			Findings: []verify.Finding{{
				Code:        "working_schema_changed",
				Message:     "the current exported DDL no longer matches the desired schema receipted by this bundle",
				Remediation: "run onwardpg draft --target " + *targetName + " --bundle " + *bundleID + " before verifying again",
			}},
		})
		return 4
	}
	report, err := verify.Run(ctx, verify.Input{
		AdminURL: adminURL, Chain: chain, BundleID: *bundleID, ThroughPhase: *through,
		Ignores: manifest.Planner.IgnoreSelectors, Options: options,
	})
	if err != nil {
		return writeError("verification_error", err)
	}
	if report.Outcome == "verified" && edited != nil && *through == "contract" {
		destination, err := config.BundlePath(root, *targetName, *bundleID)
		if err != nil {
			return writeError("invalid_bundle", err)
		}
		if err := bundle.InstallReceipts(destination, edited.Artifact); err != nil {
			return writeError("bundle_receipt_update_failed", err)
		}
		report.ReceiptsUpdated = true
	}
	_ = json.NewEncoder(os.Stdout).Encode(report)
	if report.Outcome == "verified" {
		return 0
	}
	return 4
}

func writeVerifyFinding(target, bundleID, historyHead, through, outcome, code, message, remediation string) int {
	_ = json.NewEncoder(os.Stdout).Encode(verify.Report{
		ProtocolVersion: verify.Version,
		Outcome:         outcome,
		Target:          target,
		BundleID:        bundleID,
		HistoryHead:     historyHead,
		ThroughPhase:    through,
		Findings: []verify.Finding{{
			Code: code, Message: message, Remediation: remediation,
		}},
	})
	return 4
}

func runPlan(arguments []string) int {
	flags := flag.NewFlagSet("plan", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	from, to, devURL := flags.String("from", "", ""), flags.String("to", "", ""), flags.String("dev-url", "", "")
	var inlineHints stringsFlag
	flags.Var(&inlineHints, "hint", "semantic JSON hint; repeat for multiple decisions")
	hintsFile := flags.String("hints-file", "", "JSON array of semantic hints")
	concurrentIndexes := flags.Bool("concurrent-indexes", false, "create standalone indexes concurrently")
	ifNotExists := flags.Bool("if-not-exists", false, "emit IF NOT EXISTS for schema and table creation")
	ifExists := flags.Bool("if-exists", false, "emit IF EXISTS for schema and table drops")
	cascadeDrops := flags.Bool("cascade-drops", false, "emit CASCADE for schema and table drops")
	output := flags.String("output", "json", "output format: text or json")
	unsortedDump := flags.Bool("unsorted-dump", false, "preserve dump order instead of dependency sorting")
	var schemaQualifier optionalString
	flags.Var(&schemaQualifier, "schema-qualifier", "scope to one schema and render names using this qualifier (empty means unqualified)")
	var ignores stringsFlag
	flags.Var(&ignores, "ignore", "selector to exclude")
	if err := flags.Parse(arguments); err != nil {
		return writeError("invalid_invocation", err)
	}
	if *from == "" || *to == "" {
		return writeError("invalid_invocation", errors.New("plan requires --from and --to"))
	}
	if *output != "json" && *output != "text" {
		return writeError("invalid_invocation", errors.New("plan --output must be text or json"))
	}
	if *unsortedDump {
		return writeError("invalid_invocation", errors.New("--unsorted-dump requires a complete typed object order and is unavailable for CLI URL/DDL sources"))
	}
	hints, err := readHints(inlineHints, *hintsFile)
	if err != nil {
		return writeError("invalid_hints", err)
	}
	ctx := context.Background()
	fromSpec, toSpec := source.Parse(*from), source.Parse(*to)
	current, err := source.LoadGraphForComparison(ctx, fromSpec, *devURL, ignores)
	if err != nil {
		return writeError("source_error", err)
	}
	desired, err := source.LoadGraphForComparison(ctx, toSpec, *devURL, ignores)
	if err != nil {
		return writeError("source_error", err)
	}
	if err := source.ValidateIgnoreSelectors(ignores, current, desired); err != nil {
		return writeError("invalid_ignore", err)
	}
	options := graphplan.Options{ConcurrentIndexes: *concurrentIndexes, IfNotExists: *ifNotExists, IfExists: *ifExists, CascadeDrops: *cascadeDrops, UnsortedDump: *unsortedDump}
	if schemaQualifier.set {
		options.SchemaQualifier = &schemaQualifier.value
	}
	var result protocol.Result
	if len(inlineHints) > 0 || *hintsFile != "" {
		resolution, resolveErr := semantichint.Resolve(current, desired, hints, options)
		if resolveErr != nil {
			return writeError("planning_error", resolveErr)
		}
		result = resolution.Result
	} else {
		result, err = graphplan.Build(current, desired, protocol.Answers{}, options)
		if err != nil {
			return writeError("planning_error", err)
		}
	}
	if result.Status == protocol.NeedsInput {
		decisions, decisionErr := semantichint.Decisions(result.Questions, current, desired)
		if decisionErr != nil {
			return writeError("planning_error", decisionErr)
		}
		var outputErr error
		if *output == "text" {
			outputErr = writeDecisionsText(os.Stdout, "plan", decisions)
		} else {
			outputErr = writeDecisionEnvelope(os.Stdout, "onwardpg/plan/2", decisions)
		}
		if outputErr != nil {
			return writeError("output_error", outputErr)
		}
	} else if *output == "text" {
		if result.Status == protocol.Unsupported {
			_, _ = fmt.Fprintln(os.Stdout, "unsupported")
			for _, reason := range result.Unsupported {
				_, _ = fmt.Fprintln(os.Stdout, "  "+reason)
			}
		} else {
			_, _ = fmt.Fprintln(os.Stdout, protocol.RenderSQL(result, ""))
		}
	} else {
		if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
			return writeError("output_error", err)
		}
	}
	return resultExitCode(result.Status)
}

func resultExitCode(status protocol.Status) int {
	switch status {
	case protocol.Planned:
		return 0
	case protocol.NeedsInput, protocol.NeedsSQLEdits:
		return 2
	case protocol.Unsupported:
		return 3
	default:
		return 1
	}
}

func runConfig(arguments []string) int {
	if len(arguments) == 0 || arguments[0] != "check" {
		return writeError("invalid_invocation", errors.New("usage: onwardpg config check [--config .onwardpg.toml]"))
	}
	flags := flag.NewFlagSet("config check", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	name := flags.String("config", ".onwardpg.toml", "repository configuration path")
	if err := flags.Parse(arguments[1:]); err != nil {
		return writeError("invalid_invocation", err)
	}
	configPath, err := filepath.Abs(*name)
	if err != nil {
		return writeError("invalid_config", err)
	}
	config, err := workspace.Load(configPath)
	if err != nil {
		return writeError("invalid_config", err)
	}
	targets := make([]string, 0, len(config.Targets))
	for target := range config.Targets {
		targets = append(targets, target)
	}
	sort.Strings(targets)
	type checkedTarget struct {
		Name          string `json:"name"`
		Provenance    string `json:"provenance"`
		Fingerprint   string `json:"fingerprint"`
		PostgresMajor int    `json:"postgres_major"`
	}
	checked := make([]checkedTarget, 0, len(targets))
	ctx := context.Background()
	root := filepath.Dir(configPath)
	for _, targetName := range targets {
		target := config.Targets[targetName]
		adminEnv := target.ScratchEnv()
		adminURL := os.Getenv(adminEnv)
		if adminURL == "" {
			return writeError("source_error", fmt.Errorf("target %s requires environment variable %s", targetName, adminEnv))
		}
		compiled, err := workspace.CompileDDL(ctx, root, targetName, target)
		if err != nil {
			return writeError("source_error", fmt.Errorf("target %s: %w", targetName, err))
		}
		graph, err := source.LoadDDLGraphForComparison(ctx, compiled.DDL, compiled.Provenance, adminURL, nil)
		if err != nil {
			return writeError("source_error", fmt.Errorf("target %s: %w", targetName, err))
		}
		fingerprint, err := graph.Fingerprint()
		if err != nil {
			return writeError("source_error", fmt.Errorf("target %s fingerprint: %w", targetName, err))
		}
		major, err := source.PostgresMajor(ctx, adminURL)
		if err != nil {
			return writeError("source_error", fmt.Errorf("target %s: %w", targetName, err))
		}
		checked = append(checked, checkedTarget{Name: targetName, Provenance: compiled.Provenance, Fingerprint: fingerprint, PostgresMajor: major})
	}
	_ = json.NewEncoder(os.Stdout).Encode(struct {
		ProtocolVersion string          `json:"protocol_version"`
		Status          string          `json:"status"`
		ConfigVersion   int             `json:"config_version"`
		Targets         []checkedTarget `json:"targets"`
	}{ProtocolVersion: "onwardpg.config-check/v2", Status: "valid", ConfigVersion: config.Version, Targets: checked})
	return 0
}

func readHints(inline []string, path string) ([]protocol.Hint, error) {
	var hints []protocol.Hint
	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		decoded, err := protocol.DecodeHints(data)
		if err != nil {
			return nil, err
		}
		hints = append(hints, decoded...)
	}
	for index, document := range inline {
		hint, err := protocol.DecodeHint([]byte(document))
		if err != nil {
			return nil, fmt.Errorf("inline hint %d: %w", index+1, err)
		}
		hints = append(hints, hint)
	}
	if err := protocol.ValidateHints(hints); err != nil {
		return nil, err
	}
	return hints, nil
}

func writeDraftReport(writer io.Writer, report draftflow.Report, output string) error {
	if output == "text" {
		if report.Outcome == string(protocol.NeedsInput) {
			return writeDecisionsText(writer, report.BundleID, report.Decisions)
		}
		if report.Outcome == string(protocol.NeedsSQLEdits) {
			if _, err := fmt.Fprintf(writer, "needs SQL edits: %s\n", report.Path); err != nil {
				return err
			}
			for _, name := range report.EditFiles {
				if _, err := fmt.Fprintf(writer, "  edit %s\n", name); err != nil {
					return err
				}
			}
			_, err := fmt.Fprintln(writer, "replace every ONWARDPG TODO, then run onwardpg verify")
			return err
		}
		if report.Path != "" {
			_, err := fmt.Fprintf(writer, "%s: %s\n", report.Outcome, report.Path)
			return err
		}
		_, err := fmt.Fprintln(writer, report.Outcome)
		return err
	}
	if report.Outcome == string(protocol.NeedsInput) {
		return writeDecisionEnvelope(writer, draftflow.Version, report.Decisions)
	}
	if report.Outcome == string(protocol.NeedsSQLEdits) {
		return json.NewEncoder(writer).Encode(struct {
			Protocol string   `json:"protocol"`
			Status   string   `json:"status"`
			Path     string   `json:"path"`
			Edit     []string `json:"edit"`
		}{Protocol: draftflow.Version, Status: string(protocol.NeedsSQLEdits), Path: report.Path, Edit: report.EditFiles})
	}
	return json.NewEncoder(writer).Encode(report)
}

func writeDecisionsText(writer io.Writer, subject string, decisions []protocol.Decision) error {
	if _, err := fmt.Fprintf(writer, "%s needs %d decision(s)\n", subject, len(decisions)); err != nil {
		return err
	}
	for _, decision := range decisions {
		for _, choice := range decision.Choices {
			data, err := json.Marshal(choice.Hint)
			if err != nil {
				return err
			}
			if _, err := fmt.Fprintf(writer, "  --hint %q", string(data)); err != nil {
				return err
			}
			if len(choice.Hazards) > 0 {
				if _, err := fmt.Fprintf(writer, "  hazards: %s", strings.Join(choice.Hazards, ", ")); err != nil {
					return err
				}
			}
			if _, err := fmt.Fprintln(writer); err != nil {
				return err
			}
		}
	}
	_, err := fmt.Fprintln(writer, "rerun the same command with one or more hints")
	return err
}

func writeDecisionEnvelope(writer io.Writer, version string, decisions []protocol.Decision) error {
	return json.NewEncoder(writer).Encode(struct {
		Protocol  string              `json:"protocol"`
		Status    string              `json:"status"`
		Decisions []protocol.Decision `json:"decisions"`
	}{Protocol: version, Status: "needs_decisions", Decisions: decisions})
}

func writeError(code string, err error) int {
	_ = json.NewEncoder(os.Stdout).Encode(protocol.ErrorDiagnostic(code, err))
	return 1
}

func sortedUniqueStrings(values []string) []string {
	result := append([]string(nil), values...)
	sort.Strings(result)
	if len(result) == 0 {
		return nil
	}
	write := 1
	for _, value := range result[1:] {
		if value == result[write-1] {
			continue
		}
		result[write] = value
		write++
	}
	return result[:write]
}

type stringsFlag []string

func (s *stringsFlag) String() string { return fmt.Sprint([]string(*s)) }
func (s *stringsFlag) Set(value string) error {
	if value == "" {
		return errors.New("ignore selector must not be empty")
	}
	*s = append(*s, value)
	return nil
}

// optionalString distinguishes an omitted schema qualifier from an explicitly
// empty one. The latter is Atlas-compatible unqualified rendering.
type optionalString struct {
	value string
	set   bool
}

func (s *optionalString) String() string { return s.value }
func (s *optionalString) Set(value string) error {
	s.value, s.set = value, true
	return nil
}
