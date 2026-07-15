package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime/debug"
	"sort"
	"strings"

	"github.com/jokull/onwardpg/internal/activeplan"
	"github.com/jokull/onwardpg/internal/bundle"
	"github.com/jokull/onwardpg/internal/devflow"
	"github.com/jokull/onwardpg/internal/draftflow"
	"github.com/jokull/onwardpg/internal/driftcheck"
	"github.com/jokull/onwardpg/internal/graphplan"
	"github.com/jokull/onwardpg/internal/history"
	"github.com/jokull/onwardpg/internal/historyinit"
	"github.com/jokull/onwardpg/internal/protocol"
	"github.com/jokull/onwardpg/internal/semantichint"
	"github.com/jokull/onwardpg/internal/source"
	"github.com/jokull/onwardpg/internal/targetlock"
	"github.com/jokull/onwardpg/internal/verify"
	"github.com/jokull/onwardpg/internal/workspace"
)

// buildVersion must remain a plain string initializer so release builds can
// set it with the Go linker's -X flag.
var buildVersion = "dev"

var pseudoVersionPattern = regexp.MustCompile(`[.-][0-9]{14}-[0-9a-f]{7,}(?:\+.*)?$`)

func currentBuildVersion() string {
	return selectBuildVersion(buildVersion, moduleBuildVersion())
}

func moduleBuildVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	return info.Main.Version
}

func selectBuildVersion(linkerVersion, moduleVersion string) string {
	if linkerVersion != "" && linkerVersion != "dev" {
		return linkerVersion
	}
	if moduleVersion != "" && moduleVersion != "(devel)" && !strings.Contains(moduleVersion, "+") && !pseudoVersionPattern.MatchString(moduleVersion) {
		return moduleVersion
	}
	return "dev"
}

const rootUsage = `Usage: onwardpg <command> [options]

Everyday workflow:
  config check     validate configuration, DDL, databases, and history
  init             create a replayable history ground floor
  plan             create, revise, or restack one feature migration
  status           inspect the worktree-local active feature plan
  verify           clone-verify one exact bundle
  drift check      compare replayed history with a live catalog read-only

Diagnostics and compatibility:
  diff             diff two explicit schema sources
  history status   inspect the repository-local hash chain
  dev plan         diff the development catalog against desired DDL
  draft            explicit-history compatibility bundle command
  version          print the onwardpg build version

onwardpg generates and verifies plans; it never applies them to caller databases.`

func main() { os.Exit(run()) }

func run() int {
	if len(os.Args) < 2 {
		return writeError("invalid_invocation", errors.New("usage: onwardpg <init|status|history|dev|draft|verify|drift|plan|diff|config|version>"))
	}
	switch os.Args[1] {
	case "help", "-h", "--help":
		_, _ = fmt.Fprintln(os.Stdout, rootUsage)
		return 0
	case "version", "--version":
		_ = json.NewEncoder(os.Stdout).Encode(struct {
			ProtocolVersion string `json:"protocol_version"`
			Status          string `json:"status"`
			Version         string `json:"version"`
		}{ProtocolVersion: "onwardpg.version/v1", Status: "ok", Version: currentBuildVersion()})
		return 0
	case "init":
		return runInit(os.Args[2:])
	case "status":
		return runStatusAt(os.Args[2:], ".")
	case "history":
		return runHistoryStatus(os.Args[2:])
	case "plan":
		return runPlan(os.Args[2:])
	case "diff":
		return runLowLevelPlan(os.Args[2:])
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

func runHistoryStatus(arguments []string) int { return runHistoryStatusAt(arguments, ".") }

func runStatusAt(arguments []string, start string) int {
	if helpRequested(arguments) {
		_, _ = fmt.Fprintln(os.Stdout, "Usage: onwardpg status [--target NAME]")
		return 0
	}
	flags := flag.NewFlagSet("status", flag.ContinueOnError)
	targetName := flags.String("target", "", "configured database target name")
	configName := flags.String("config", ".onwardpg.toml", "repository configuration path")
	if help, err := parseFlagSet(flags, arguments); help {
		return 0
	} else if err != nil {
		return writeError("invalid_invocation", err)
	}
	if code := rejectPositionals(flags, "status"); code != 0 {
		return code
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
	if _, err := resolveConfiguredTarget(config, targetName); err != nil {
		return writeError("invalid_config", err)
	}
	root := filepath.Dir(configPath)
	anchor, found, err := activeplan.Load(root, *targetName)
	if err != nil {
		return writeError("active_plan_error", err)
	}
	if !found {
		_ = json.NewEncoder(os.Stdout).Encode(struct {
			ProtocolVersion string `json:"protocol_version"`
			Status          string `json:"status"`
			Target          string `json:"target"`
		}{ProtocolVersion: "onwardpg.status/v1", Status: "no_active_plan", Target: *targetName})
		return 0
	}
	historyStatus, err := history.Inspect(root, config.BundleRoot, *targetName, anchor.BundleID)
	if err != nil {
		_ = json.NewEncoder(os.Stdout).Encode(struct {
			ProtocolVersion string                  `json:"protocol_version"`
			Status          string                  `json:"status"`
			Target          string                  `json:"target"`
			Plan            activeplan.Anchor       `json:"plan"`
			Findings        []history.StatusFinding `json:"findings"`
		}{
			ProtocolVersion: "onwardpg.status/v1", Status: "blocked", Target: *targetName, Plan: anchor,
			Findings: []history.StatusFinding{{Code: "invalid_history", Message: err.Error(), Remediation: "restore one replayable accepted chain, then run onwardpg plan"}},
		})
		return 4
	}
	_ = json.NewEncoder(os.Stdout).Encode(struct {
		ProtocolVersion string               `json:"protocol_version"`
		Status          string               `json:"status"`
		Target          string               `json:"target"`
		Plan            activeplan.Anchor    `json:"plan"`
		History         history.StatusReport `json:"history"`
	}{
		ProtocolVersion: "onwardpg.status/v1", Status: historyStatus.Status, Target: *targetName, Plan: anchor, History: historyStatus,
	})
	if historyStatus.Status == "valid" || historyStatus.Status == "missing" {
		return 0
	}
	return 4
}

func runHistoryStatusAt(arguments []string, start string) int {
	if helpRequested(arguments) {
		_, _ = fmt.Fprintln(os.Stdout, "Usage: onwardpg history status [--target NAME] [--bundle ID]")
		return 0
	}
	if len(arguments) == 0 || arguments[0] != "status" {
		return writeError("invalid_invocation", errors.New("usage: onwardpg history status [--target NAME] [--bundle ID]"))
	}
	flags := flag.NewFlagSet("history status", flag.ContinueOnError)
	targetName := flags.String("target", "", "configured database target name")
	bundleID := flags.String("bundle", "", "selected mutable bundle to exclude from its base")
	configName := flags.String("config", ".onwardpg.toml", "repository configuration path")
	if help, err := parseFlagSet(flags, arguments[1:]); help {
		return 0
	} else if err != nil {
		return writeError("invalid_invocation", err)
	}
	if code := rejectPositionals(flags, "history status"); code != 0 {
		return code
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
	if _, err := resolveConfiguredTarget(config, targetName); err != nil {
		return writeError("invalid_config", err)
	}
	report, err := history.Inspect(filepath.Dir(configPath), config.BundleRoot, *targetName, *bundleID)
	if err != nil {
		_ = json.NewEncoder(os.Stdout).Encode(history.StatusReport{
			ProtocolVersion: history.StatusVersion, Status: "blocked", Target: *targetName,
			Findings: []history.StatusFinding{{
				Code: "invalid_history", Message: err.Error(),
				Remediation: "restore one complete hash-chained target history before drafting or verifying",
			}},
		})
		return 4
	}
	_ = json.NewEncoder(os.Stdout).Encode(report)
	if report.Status == "valid" {
		return 0
	}
	return 4
}

func runDrift(arguments []string) int { return runDriftAt(arguments, ".") }

func runDriftAt(arguments []string, start string) int {
	if helpRequested(arguments) {
		_, _ = fmt.Fprintln(os.Stdout, "Usage: onwardpg drift check --database URL [--target NAME]")
		return 0
	}
	if len(arguments) == 0 || arguments[0] != "check" {
		return writeError("invalid_invocation", errors.New("usage: onwardpg drift check --database URL [--target NAME]"))
	}
	flags := flag.NewFlagSet("drift check", flag.ContinueOnError)
	targetName := flags.String("target", "", "configured database target name")
	databaseURL := flags.String("database", "", "live PostgreSQL URL inspected read-only")
	configName := flags.String("config", ".onwardpg.toml", "repository configuration path")
	var ignores stringsFlag
	flags.Var(&ignores, "ignore", "validated catalog selector to exclude")
	if help, err := parseFlagSet(flags, arguments[1:]); help {
		return 0
	} else if err != nil {
		return writeError("invalid_invocation", err)
	}
	if code := rejectPositionals(flags, "drift check"); code != 0 {
		return code
	}
	if *databaseURL == "" {
		return writeError("invalid_invocation", errors.New("drift check requires --database"))
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
	target, err := resolveConfiguredTarget(config, targetName)
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
	selectors := targetIgnoreSelectors(target, ignores)
	expected, err := source.LoadDDLGraphForComparison(ctx, replay.DDL, replay.Provenance, scratchURL, selectors)
	if err != nil {
		return writeError("source_error", fmt.Errorf("replay expected history: %w", err))
	}
	actual, err := source.LoadGraphForComparison(ctx, source.Parse(*databaseURL), "", selectors)
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
		return writeError("invalid_invocation", errors.New("usage: onwardpg init [--target NAME] [--bundle baseline]"))
	}
	flags := flag.NewFlagSet("init", flag.ContinueOnError)
	targetName := flags.String("target", "", "configured database target name")
	bundleID := flags.String("bundle", "baseline", "root history bundle identifier")
	configName := flags.String("config", ".onwardpg.toml", "repository configuration path")
	concurrentIndexes := flags.Bool("concurrent-indexes", false, "create standalone indexes concurrently")
	var ignores stringsFlag
	flags.Var(&ignores, "ignore", "validated catalog selector to exclude")
	if help, err := parseFlagSet(flags, arguments[1:]); help {
		return 0
	} else if err != nil {
		return writeError("invalid_invocation", err)
	}
	if code := rejectPositionals(flags, "init"); code != 0 {
		return code
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
	target, err := resolveConfiguredTarget(config, targetName)
	if err != nil {
		return writeError("invalid_config", err)
	}
	adminEnv := target.ScratchEnv()
	adminURL := os.Getenv(adminEnv)
	if adminURL == "" {
		return writeError("source_error", fmt.Errorf("environment variable %s is required", adminEnv))
	}
	selectors := targetIgnoreSelectors(target, ignores)
	report, err := historyinit.Run(context.Background(), historyinit.Input{
		Root:            filepath.Dir(configPath),
		ConfigPath:      configPath,
		Config:          config,
		TargetName:      *targetName,
		Target:          target,
		AdminURL:        adminURL,
		BundleID:        *bundleID,
		BuildVersion:    currentBuildVersion(),
		Ignores:         selectors,
		RequiredIgnores: sortedUniqueStrings(ignores),
		PlannerOptions:  graphplan.Options{ConcurrentIndexes: *concurrentIndexes},
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
	if helpRequested(arguments) {
		_, _ = fmt.Fprintln(os.Stdout, "Usage: onwardpg dev plan [--target NAME] [--hint JSON] [--hints-file FILE] [--output text|json]")
		return 0
	}
	if len(arguments) == 0 || arguments[0] != "plan" {
		return writeError("invalid_invocation", errors.New("usage: onwardpg dev plan [--target NAME] [--hint JSON] [--hints-file FILE] [--output text|json]"))
	}
	flags := flag.NewFlagSet("dev plan", flag.ContinueOnError)
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
	if help, err := parseFlagSet(flags, arguments[1:]); help {
		return 0
	} else if err != nil {
		return writeError("invalid_invocation", err)
	}
	if code := rejectPositionals(flags, "dev plan"); code != 0 {
		return code
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
	target, err := resolveConfiguredTarget(config, targetName)
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
	options := graphplan.Options{
		ConcurrentIndexes: *concurrentIndexes, IfNotExists: *ifNotExists,
		IfExists: *ifExists, CascadeDrops: *cascadeDrops,
	}
	if schemaQualifier.set {
		options.SchemaQualifier = &schemaQualifier.value
	}
	report, err := devflow.Run(context.Background(), devflow.Input{
		Root: filepath.Dir(configPath), TargetName: *targetName, Target: target, DevURL: devURL, AdminURL: scratchURL,
		Hints: hints, Ignores: targetIgnoreSelectors(target, ignores), RequiredIgnores: sortedUniqueStrings(ignores), PlannerOptions: options,
	})
	if err != nil {
		return writeError("planning_error", err)
	}
	result := report.Result
	if result.Status == protocol.NeedsInput {
		var outputErr error
		if *output == "text" {
			outputErr = writeDecisionsText(os.Stdout, "dev plan", report.Decisions)
		} else {
			outputErr = writeDecisionEnvelope(os.Stdout, devflow.Version, report.Decisions, result.Analysis)
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
		} else if len(result.Statements) == 0 {
			if len(result.Preserved) == 0 && len(result.Compatibility) == 0 {
				_, _ = fmt.Fprintln(os.Stdout, "-- onwardpg: no changes")
			} else {
				_, _ = fmt.Fprintln(os.Stdout, "-- onwardpg: workspace compatible; retained differences")
				for _, object := range result.Preserved {
					_, _ = fmt.Fprintln(os.Stdout, "-- preserved: "+object)
				}
				for _, difference := range result.Compatibility {
					_, _ = fmt.Fprintln(os.Stdout, "-- workspace compatibility retained: "+difference)
				}
			}
		} else {
			_, _ = fmt.Fprintln(os.Stdout, protocol.RenderSQL(result, ""))
		}
	} else {
		if result.Status == protocol.Planned && len(result.Statements) == 0 {
			_ = json.NewEncoder(os.Stdout).Encode(struct {
				ProtocolVersion    string   `json:"protocol_version"`
				Status             string   `json:"status"`
				Changed            bool     `json:"changed"`
				CurrentFingerprint string   `json:"current_fingerprint"`
				DesiredFingerprint string   `json:"desired_fingerprint"`
				Preserved          []string `json:"preserved,omitempty"`
			}{
				ProtocolVersion: devflow.Version, Status: devNoChangeStatus(result), Changed: false,
				CurrentFingerprint: result.CurrentFingerprint, DesiredFingerprint: result.DesiredFingerprint,
				Preserved: result.Preserved,
			})
		} else {
			publicResult := result
			publicResult.ProtocolVersion = devflow.Version
			_ = json.NewEncoder(os.Stdout).Encode(publicResult)
		}
	}
	return resultExitCode(result.Status)
}

func devNoChangeStatus(result protocol.Result) string {
	if len(result.Preserved) > 0 || len(result.Compatibility) > 0 {
		return "workspace_compatible"
	}
	return "no_changes"
}

func runDraft(arguments []string) int { return runDraftAt(arguments, ".") }

func runDraftAt(arguments []string, start string) int {
	flags := flag.NewFlagSet("draft", flag.ContinueOnError)
	targetName := flags.String("target", "", "configured database target name")
	bundleID := flags.String("bundle", "", "stable logical feature bundle identifier")
	afterRef := flags.String("after", "", "exact accepted head_ref this feature bundle must follow")
	create := flags.Bool("create", false, "assert this is the bundle's first invocation")
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
	if help, err := parseFlagSet(flags, arguments); help {
		return 0
	} else if err != nil {
		return writeError("invalid_invocation", err)
	}
	if *bundleID == "" {
		return writeError("invalid_invocation", errors.New("draft requires --bundle"))
	}
	if code := rejectPositionals(flags, "draft"); code != 0 {
		return code
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
	target, err := resolveConfiguredTarget(config, targetName)
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
		Root:            filepath.Dir(configPath),
		ConfigPath:      configPath,
		Config:          config,
		TargetName:      *targetName,
		Target:          target,
		AdminURL:        adminURL,
		BundleID:        *bundleID,
		AfterRef:        *afterRef,
		Create:          *create,
		BuildVersion:    currentBuildVersion(),
		Purpose:         *purpose,
		Hints:           hints,
		HintsGiven:      len(inlineHints) > 0 || *hintsFile != "",
		Ignores:         targetIgnoreSelectors(target, ignores),
		RequiredIgnores: sortedUniqueStrings(ignores),
		PlannerOptions:  options,
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
	case "no_changes", "absorbed":
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
		return writeError("invalid_invocation", errors.New("usage: onwardpg verify [--target NAME] [--bundle ID] [--through PHASE]"))
	}
	flags := flag.NewFlagSet("verify", flag.ContinueOnError)
	targetName := flags.String("target", "", "configured database target name")
	bundleID := flags.String("bundle", "", "bundle to verify")
	through := flags.String("through", "contract", "last phase to execute: expand or contract")
	check := flags.Bool("check", false, "read-only verification; reject unreceipted edits")
	configName := flags.String("config", ".onwardpg.toml", "repository configuration path")
	if help, err := parseFlagSet(flags, arguments[1:]); help {
		return 0
	} else if err != nil {
		return writeError("invalid_invocation", err)
	}
	if code := rejectPositionals(flags, "verify"); code != 0 {
		return code
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
	target, err := resolveConfiguredTarget(config, targetName)
	if err != nil {
		return writeError("invalid_config", err)
	}
	root := filepath.Dir(configPath)
	var selectedAnchor *activeplan.Anchor
	if *bundleID == "" {
		anchor, found, anchorErr := activeplan.Load(root, *targetName)
		if anchorErr != nil {
			return writeError("active_plan_error", anchorErr)
		}
		if !found {
			return writeError("active_plan_required", errors.New("verify needs --bundle outside a workspace with an active onwardpg plan"))
		}
		*bundleID = anchor.BundleID
		selectedAnchor = &anchor
	}
	adminEnv := target.ScratchEnv()
	adminURL := os.Getenv(adminEnv)
	if adminURL == "" {
		return writeError("source_error", fmt.Errorf("environment variable %s is required", adminEnv))
	}
	lock, err := targetlock.Acquire(configPath, *targetName)
	if err != nil {
		if errors.Is(err, targetlock.ErrBusy) {
			return writeVerifyFinding(*targetName, *bundleID, "", *through, "blocked", "history_update_in_progress",
				err.Error(), "wait for the other command and retry; the operating system releases this lock automatically if its owner exits")
		}
		return writeError("history_lock_error", err)
	}
	defer lock.Release()
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
				"identify the accepted head with onwardpg history status --target "+*targetName+" --bundle "+*bundleID+", then rerun draft with the same bundle and --after <head_ref>")
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
	if selectedAnchor != nil && manifest.PlanID != selectedAnchor.PlanID {
		return writeVerifyFinding(*targetName, *bundleID, prefix.HeadDigest, *through, "blocked", "active_plan_identity_mismatch",
			fmt.Sprintf("local active plan %s does not match bundle plan id %s", selectedAnchor.PlanID, manifest.PlanID),
			"select the intended bundle explicitly or remove the stale local active-plan anchor")
	}
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
				Remediation: "identify the accepted head with onwardpg history status --target " + *targetName + " --bundle " + *bundleID + ", then rerun draft with the same bundle and --after <head_ref>",
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
	if report.Outcome == "verified" || report.Outcome == "partial_verified" {
		if lockErr := lock.ValidatePath(); lockErr != nil {
			return writeVerifyFinding(*targetName, *bundleID, chain.HeadDigest, *through, "blocked", "configuration_changed_during_verify",
				lockErr.Error(),
				"rerun verification against the current stable configuration; no receipts were installed")
		}
		if configErr := workspace.RequireUnchanged(configPath, config); configErr != nil {
			return writeVerifyFinding(*targetName, *bundleID, chain.HeadDigest, *through, "blocked", "configuration_changed_during_verify",
				configErr.Error(),
				"rerun verification against the current stable configuration; no receipts were installed")
		}
		compiledAfter, compileErr := workspace.CompileDDL(ctx, root, *targetName, target)
		if compileErr != nil {
			return writeError("source_error", fmt.Errorf("recompile desired schema after verification: %w", compileErr))
		}
		workingAfter, loadErr := source.LoadDDLGraphForComparison(ctx, compiledAfter.DDL, compiledAfter.Provenance, adminURL, manifest.Planner.IgnoreSelectors)
		if loadErr != nil {
			return writeError("source_error", fmt.Errorf("rematerialize desired schema after verification: %w", loadErr))
		}
		workingAfterFingerprint, fingerprintErr := workingAfter.Fingerprint()
		if fingerprintErr != nil {
			return writeError("source_error", fmt.Errorf("fingerprint desired schema after verification: %w", fingerprintErr))
		}
		if workingAfterFingerprint != workingFingerprint {
			return writeVerifyFinding(*targetName, *bundleID, chain.HeadDigest, *through, "blocked", "working_schema_changed_during_verify",
				fmt.Sprintf("configured desired schema changed from %s to %s while clone verification was running", workingFingerprint, workingAfterFingerprint),
				"rerun draft against the current exported DDL before verifying again; no receipts were installed")
		}
		if edited == nil {
			latest, loadErr := history.Load(root, config.BundleRoot, *targetName)
			if loadErr != nil || !reflect.DeepEqual(chain, latest) {
				message := "target history changed while clone verification was running"
				if loadErr != nil {
					message += ": " + loadErr.Error()
				}
				return writeVerifyFinding(*targetName, *bundleID, chain.HeadDigest, *through, "blocked", "history_changed_during_verify",
					message, "inspect history status and rerun verification against the current exact chain")
			}
		} else {
			latestBase, latestSelected, loadErr := history.LoadEditedDraft(root, config.BundleRoot, *targetName, *bundleID)
			if loadErr != nil || latestSelected == nil || latestBase.HeadDigest != edited.Artifact.Manifest.History.ParentDigest || !reflect.DeepEqual(latestSelected.Artifact, edited.Artifact) {
				message := "target history or edited SQL changed while clone verification was running"
				if loadErr != nil {
					message += ": " + loadErr.Error()
				}
				return writeVerifyFinding(*targetName, *bundleID, chain.HeadDigest, *through, "blocked", "history_changed_during_verify",
					message, "inspect the edited bundle and rerun verification; no receipts were installed")
			}
		}
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
	if report.Outcome == "verified" || report.Outcome == "partial_verified" {
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

// runPlan is the ergonomic lifecycle command. The old explicit --from/--to
// spelling remains accepted here for one preview transition and is available
// permanently as `onwardpg diff`.
func runPlan(arguments []string) int {
	for _, argument := range arguments {
		if argument == "--from" || argument == "--to" || strings.HasPrefix(argument, "--from=") || strings.HasPrefix(argument, "--to=") {
			return runLowLevelPlan(arguments)
		}
	}
	return runWorkflowPlanAt(arguments, ".")
}

func runWorkflowPlanAt(arguments []string, start string) int {
	if helpRequested(arguments) {
		_, _ = fmt.Fprintln(os.Stdout, "Usage: onwardpg plan [NAME] [--target NAME] [--bundle ID] [--hint JSON] [--dev-hint JSON] [--output sql|text|json]")
		return 0
	}
	name := ""
	if len(arguments) > 0 && !strings.HasPrefix(arguments[0], "-") {
		name = arguments[0]
		arguments = arguments[1:]
	}
	flags := flag.NewFlagSet("plan", flag.ContinueOnError)
	targetName := flags.String("target", "", "configured database target name")
	bundleID := flags.String("bundle", "", "explicit existing feature bundle identifier")
	configName := flags.String("config", ".onwardpg.toml", "repository configuration path")
	var inlineHints stringsFlag
	flags.Var(&inlineHints, "hint", "semantic JSON hint; repeat for multiple decisions")
	hintsFile := flags.String("hints-file", "", "JSON array of semantic hints")
	var inlineDevHints stringsFlag
	flags.Var(&inlineDevHints, "dev-hint", "development-workspace semantic JSON hint; repeat for multiple decisions")
	devHintsFile := flags.String("dev-hints-file", "", "JSON array of development-workspace semantic hints")
	output := flags.String("output", "json", "output format: sql, text, or json")
	purpose := flags.String("purpose", "feature", "feature, repair, or contract")
	concurrentIndexes := flags.Bool("concurrent-indexes", false, "create standalone indexes concurrently")
	ifNotExists := flags.Bool("if-not-exists", false, "emit IF NOT EXISTS for schema and table creation")
	ifExists := flags.Bool("if-exists", false, "emit IF EXISTS for schema and table drops")
	cascadeDrops := flags.Bool("cascade-drops", false, "emit CASCADE for schema and table drops")
	var schemaQualifier optionalString
	flags.Var(&schemaQualifier, "schema-qualifier", "scope to one schema and render names using this qualifier")
	var ignores stringsFlag
	flags.Var(&ignores, "ignore", "validated catalog selector to exclude")
	if help, err := parseFlagSet(flags, arguments); help {
		return 0
	} else if err != nil {
		return writeError("invalid_invocation", err)
	}
	if code := rejectPositionals(flags, "plan"); code != 0 {
		return code
	}
	if name != "" && *bundleID != "" {
		return writeError("invalid_invocation", errors.New("plan accepts either NAME or --bundle, not both"))
	}
	if *output != "json" && *output != "text" && *output != "sql" {
		return writeError("invalid_invocation", errors.New("plan --output must be sql, text, or json"))
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
	target, err := resolveConfiguredTarget(config, targetName)
	if err != nil {
		return writeError("invalid_config", err)
	}
	root := filepath.Dir(configPath)
	anchor, anchored, err := activeplan.Load(root, *targetName)
	if err != nil {
		return writeError("active_plan_error", err)
	}
	var planID string
	createBundle := false
	parked := []activeplan.SavedPlan(nil)
	if anchored {
		parked = append(parked, anchor.Parked...)
	}
	switch {
	case name != "":
		if anchored && anchor.BundleID != name {
			present, presentErr := bundleDirectoryPresent(config, root, *targetName, anchor.BundleID)
			if presentErr != nil {
				return writeError("active_plan_error", presentErr)
			}
			if present {
				return writeWorkflowPlanFinding(*targetName, "", "blocked", "active_plan_exists",
					fmt.Sprintf("local workspace already has active plan %s in bundle %s", anchor.PlanID, anchor.BundleID),
					"continue it with onwardpg plan, or switch to a checkout where its bundle is absent before starting another local plan")
			}
			if saved, exists := anchor.FindParked(name); exists {
				planID = saved.PlanID
			} else {
				planID, err = activeplan.NewID()
				if err != nil {
					return writeError("active_plan_error", err)
				}
				createBundle = true
			}
			parked = anchor.WithActive(activeplan.SavedPlan{PlanID: planID, BundleID: name}).Parked
			*bundleID = name
		} else if anchored {
			*bundleID, planID = anchor.BundleID, anchor.PlanID
		} else {
			*bundleID = name
			planID, err = activeplan.NewID()
			if err != nil {
				return writeError("active_plan_error", err)
			}
			createBundle = true
		}
	case *bundleID != "":
		if anchored && anchor.BundleID == *bundleID {
			planID = anchor.PlanID
		} else {
			if anchored {
				present, presentErr := bundleDirectoryPresent(config, root, *targetName, anchor.BundleID)
				if presentErr != nil {
					return writeError("active_plan_error", presentErr)
				}
				if present {
					return writeWorkflowPlanFinding(*targetName, *bundleID, "blocked", "active_plan_exists",
						fmt.Sprintf("local workspace already has active plan %s in bundle %s", anchor.PlanID, anchor.BundleID),
						"continue the active plan, or switch to a checkout where its bundle is absent before selecting another plan")
				}
			}
			destination, pathErr := config.BundlePath(root, *targetName, *bundleID)
			if pathErr != nil {
				return writeError("invalid_bundle", pathErr)
			}
			artifact, readErr := bundle.Read(destination)
			if readErr != nil {
				return writeWorkflowPlanFinding(*targetName, *bundleID, "blocked", "selected_bundle_unavailable",
					readErr.Error(), "select an existing receipted onwardpg plan bundle or start a new one with onwardpg plan NAME")
			}
			if artifact.Manifest.PlanID == "" {
				return writeWorkflowPlanFinding(*targetName, *bundleID, "blocked", "selected_bundle_has_no_plan_id",
					"selected bundle predates active-plan identity", "continue it with onwardpg draft, or start a new onwardpg plan NAME")
			}
			planID = artifact.Manifest.PlanID
			if anchored {
				parked = anchor.WithActive(activeplan.SavedPlan{PlanID: planID, BundleID: *bundleID}).Parked
			}
		}
	case anchored:
		*bundleID, planID = anchor.BundleID, anchor.PlanID
	default:
		return writeWorkflowPlanFinding(*targetName, "", "blocked", "active_plan_required",
			"this workspace has no active plan", "start one with onwardpg plan NAME")
	}
	adminURL := os.Getenv(target.ScratchEnv())
	if adminURL == "" {
		return writeError("source_error", fmt.Errorf("environment variable %s is required", target.ScratchEnv()))
	}
	hints, err := readHints(inlineHints, *hintsFile)
	if err != nil {
		return writeError("invalid_hints", err)
	}
	devHints, err := readHints(inlineDevHints, *devHintsFile)
	if err != nil {
		return writeError("invalid_development_hints", err)
	}
	options := graphplan.Options{ConcurrentIndexes: *concurrentIndexes, IfNotExists: *ifNotExists, IfExists: *ifExists, CascadeDrops: *cascadeDrops}
	if schemaQualifier.set {
		options.SchemaQualifier = &schemaQualifier.value
	}
	devURL := os.Getenv(target.DevDatabaseEnv)
	if devURL == "" {
		return writeError("source_error", fmt.Errorf("environment variable %s is required", target.DevDatabaseEnv))
	}
	report, err := draftflow.Run(context.Background(), draftflow.Input{
		Root: root, ConfigPath: configPath, Config: config, TargetName: *targetName, Target: target,
		AdminURL: adminURL, BundleID: *bundleID, PlanID: planID, InferBase: true,
		Create: createBundle, BuildVersion: currentBuildVersion(), Purpose: *purpose,
		Hints: hints, HintsGiven: len(inlineHints) > 0 || *hintsFile != "", Ignores: targetIgnoreSelectors(target, ignores), RequiredIgnores: sortedUniqueStrings(ignores), PlannerOptions: options,
	})
	if err != nil {
		return writeError("plan_error", err)
	}
	// Durable H -> W state is authoritative and independently resumable. Store
	// its local selector before the optional companion D -> W inspection so an
	// unavailable development database cannot strand a written bundle without
	// an active PlanID.
	if report.RemovedBundle {
		if err := activeplan.Clear(root, *targetName); err != nil {
			return writeError("active_plan_error", err)
		}
	} else if report.Path != "" {
		if err := activeplan.Store(root, activeplan.Anchor{Version: activeplan.Version, Target: *targetName, PlanID: planID, BundleID: *bundleID, Parked: parked}); err != nil {
			return writeError("active_plan_error", err)
		}
	}
	// Durable semantic hints are intentionally not forwarded blindly to D -> W.
	// The source graph can differ from H, so an otherwise valid durable rename
	// hint may be unused locally. Devflow reports its own scoped ambiguity.
	development, err := devflow.Run(context.Background(), devflow.Input{
		Root: root, TargetName: *targetName, Target: target, DevURL: devURL, AdminURL: adminURL,
		Hints: devHints, Ignores: targetIgnoreSelectors(target, ignores), RequiredIgnores: sortedUniqueStrings(ignores), PlannerOptions: options,
		Postconditions: developmentPostconditions(report.DevelopmentPostconditions),
	})
	if err != nil {
		return writeError("development_plan_error", err)
	}
	if development.Status == protocol.NeedsInput {
		development.NextAction = "rerun_plan_with_dev_hints"
	}
	if err := writeWorkflowPlanReport(os.Stdout, *output, report, development); err != nil {
		return writeError("output_error", err)
	}
	return workflowPlanExitCode(report.Outcome, development)
}

func developmentPostconditions(checks []draftflow.DevelopmentPostcondition) []devflow.Postcondition {
	result := make([]devflow.Postcondition, 0, len(checks))
	for _, check := range checks {
		result = append(result, devflow.Postcondition{BundleID: check.BundleID, Path: check.Path, ID: check.ID, SQL: check.SQL})
	}
	return result
}

type workflowPlanReport struct {
	ProtocolVersion string           `json:"protocol_version"`
	Status          string           `json:"status"`
	Durable         draftflow.Report `json:"durable"`
	Development     devflow.Report   `json:"development"`
}

func writeWorkflowPlanReport(writer io.Writer, output string, durable draftflow.Report, development devflow.Report) error {
	if output == "sql" {
		if durable.Outcome == string(protocol.Planned) && development.Status == protocol.Planned {
			if len(development.Result.Statements) == 0 {
				if len(development.Result.Preserved) == 0 && len(development.Result.Compatibility) == 0 {
					if _, err := fmt.Fprintln(writer, "-- onwardpg: development workspace already satisfies the desired schema"); err != nil {
						return err
					}
				} else {
					if _, err := fmt.Fprintln(writer, "-- onwardpg: development workspace compatible; retained differences"); err != nil {
						return err
					}
					for _, object := range development.Result.Preserved {
						if _, err := fmt.Fprintln(writer, "-- preserved: "+object); err != nil {
							return err
						}
					}
					for _, difference := range development.Result.Compatibility {
						if _, err := fmt.Fprintln(writer, "-- workspace compatibility retained: "+difference); err != nil {
							return err
						}
					}
				}
				return writeDevelopmentEvidenceComments(writer, durable.AcceptedSteps, development.Postconditions)
			}
			if _, err := fmt.Fprint(writer, "-- onwardpg development workspace reconciliation\n-- this is not the cumulative PR migration\n"); err != nil {
				return err
			}
			if err := writeDevelopmentEvidenceComments(writer, durable.AcceptedSteps, development.Postconditions); err != nil {
				return err
			}
			_, err := fmt.Fprint(writer, "\n"+protocol.RenderSQL(development.Result, ""))
			return err
		}
		// Never mix an incomplete executable stream with human or JSON data.
		return json.NewEncoder(os.Stderr).Encode(workflowPlanReport{
			ProtocolVersion: "onwardpg.plan/v5", Status: workflowPlanStatus(durable.Outcome, development), Durable: durable, Development: development,
		})
	}
	if output == "text" {
		if err := writeDraftReport(writer, durable, "text"); err != nil {
			return err
		}
		if development.Status == protocol.NeedsInput {
			_, _ = fmt.Fprintln(writer, "\ndevelopment workspace decisions:")
			return writeDecisionsTextWithFlag(writer, "development workspace", "--dev-hint", development.Decisions)
		}
		if development.Status == protocol.Planned && len(development.Result.Statements) > 0 {
			_, _ = fmt.Fprintln(writer, "\ndevelopment workspace SQL:")
			if _, err := fmt.Fprintln(writer, protocol.RenderSQL(development.Result, "")); err != nil {
				return err
			}
		}
		if len(development.Postconditions) > 0 {
			_, _ = fmt.Fprintln(writer, "\naccepted development postconditions:")
			for _, check := range development.Postconditions {
				line := check.Status + " " + check.Path + "#" + check.ID
				if check.Message != "" {
					line += " — " + check.Message
				}
				if _, err := fmt.Fprintln(writer, "  "+line); err != nil {
					return err
				}
			}
		}
		return nil
	}
	return json.NewEncoder(writer).Encode(workflowPlanReport{
		ProtocolVersion: "onwardpg.plan/v5", Status: workflowPlanStatus(durable.Outcome, development), Durable: durable, Development: development,
	})
}

func writeDevelopmentEvidenceComments(writer io.Writer, steps []draftflow.AcceptedStep, checks []devflow.PostconditionResult) error {
	for _, step := range steps {
		if !step.RequiresReview {
			continue
		}
		if _, err := fmt.Fprintln(writer, "-- review accepted work not provable from catalog ("+step.Kind+"): "+step.Path); err != nil {
			return err
		}
	}
	for _, check := range checks {
		line := "-- accepted development postcondition " + check.Status + ": " + check.Path + "#" + check.ID
		if check.Message != "" {
			line += " (" + check.Message + ")"
		}
		if _, err := fmt.Fprintln(writer, line); err != nil {
			return err
		}
	}
	return nil
}

func workflowPlanStatus(durable string, development devflow.Report) string {
	if durable != string(protocol.Planned) && durable != "no_changes" && durable != "absorbed" {
		return durable
	}
	if development.Status == protocol.NeedsInput {
		return "needs_development_decisions"
	}
	if development.Status == protocol.Unsupported {
		return "development_unsupported"
	}
	if failedDevelopmentPostconditions(development.Postconditions) {
		return "needs_development_review"
	}
	if development.Status == protocol.Planned && len(development.Result.Statements) == 0 && (len(development.Result.Preserved) > 0 || len(development.Result.Compatibility) > 0) {
		return "workspace_compatible"
	}
	return durable
}

func workflowPlanExitCode(durable string, development devflow.Report) int {
	switch durable {
	case "blocked":
		return 4
	case string(protocol.Unsupported):
		return 3
	case string(protocol.NeedsInput), string(protocol.NeedsSQLEdits):
		return 2
	case string(protocol.Planned), "no_changes", "absorbed":
		if failedDevelopmentPostconditions(development.Postconditions) {
			return 2
		}
		return resultExitCode(development.Status)
	default:
		return 1
	}
}

func failedDevelopmentPostconditions(checks []devflow.PostconditionResult) bool {
	for _, check := range checks {
		if check.Status != "passed" {
			return true
		}
	}
	return false
}

func writeWorkflowPlanFinding(target, bundleID, outcome, code, message, remediation string) int {
	_ = json.NewEncoder(os.Stdout).Encode(struct {
		ProtocolVersion string              `json:"protocol_version"`
		Status          string              `json:"status"`
		Target          string              `json:"target"`
		BundleID        string              `json:"bundle_id,omitempty"`
		Findings        []draftflow.Finding `json:"findings"`
	}{
		ProtocolVersion: "onwardpg.plan/v5", Status: outcome, Target: target, BundleID: bundleID,
		Findings: []draftflow.Finding{{Code: code, Message: message, Remediation: remediation}},
	})
	return 4
}

// bundleDirectoryPresent is intentionally a filesystem-only check. It lets a
// worktree-local anchor notice that its bundle disappeared after a branch
// switch, without inspecting Git or treating a branch name as correctness
// evidence. A present directory is conservatively considered active even when
// its contents are broken; callers must repair it rather than overwrite it.
func bundleDirectoryPresent(config workspace.Config, root, target, bundleID string) (bool, error) {
	path, err := config.BundlePath(root, target, bundleID)
	if err != nil {
		return false, err
	}
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect active bundle directory: %w", err)
	}
	return info.IsDir(), nil
}

func runLowLevelPlan(arguments []string) int {
	flags := flag.NewFlagSet("plan", flag.ContinueOnError)
	from := flags.String("from", "", "current PostgreSQL URL or CREATE-statement SQL file")
	to := flags.String("to", "", "desired PostgreSQL URL or CREATE-statement SQL file")
	devURL := flags.String("dev-url", "", "PostgreSQL admin URL for disposable materialization databases")
	var inlineHints stringsFlag
	flags.Var(&inlineHints, "hint", "semantic JSON hint; repeat for multiple decisions")
	hintsFile := flags.String("hints-file", "", "JSON array of semantic hints")
	concurrentIndexes := flags.Bool("concurrent-indexes", false, "create standalone indexes concurrently")
	ifNotExists := flags.Bool("if-not-exists", false, "emit IF NOT EXISTS for schema and table creation")
	ifExists := flags.Bool("if-exists", false, "emit IF EXISTS for schema and table drops")
	cascadeDrops := flags.Bool("cascade-drops", false, "emit CASCADE for schema and table drops")
	output := flags.String("output", "json", "output format: text or json")
	var schemaQualifier optionalString
	flags.Var(&schemaQualifier, "schema-qualifier", "scope to one schema and render names using this qualifier (empty means unqualified)")
	var ignores stringsFlag
	flags.Var(&ignores, "ignore", "selector to exclude")
	if help, err := parseFlagSet(flags, arguments); help {
		return 0
	} else if err != nil {
		return writeError("invalid_invocation", err)
	}
	if code := rejectPositionals(flags, "plan"); code != 0 {
		return code
	}
	if *from == "" || *to == "" {
		return writeError("invalid_invocation", errors.New("plan requires --from and --to"))
	}
	if *output != "json" && *output != "text" {
		return writeError("invalid_invocation", errors.New("plan --output must be text or json"))
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
	options := graphplan.Options{ConcurrentIndexes: *concurrentIndexes, IfNotExists: *ifNotExists, IfExists: *ifExists, CascadeDrops: *cascadeDrops}
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
			outputErr = writeDecisionEnvelope(os.Stdout, "onwardpg.plan/v4", decisions, result.Analysis)
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
		publicResult := result
		publicResult.ProtocolVersion = "onwardpg.plan/v4"
		if err := json.NewEncoder(os.Stdout).Encode(publicResult); err != nil {
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
	if helpRequested(arguments) {
		_, _ = fmt.Fprintln(os.Stdout, "Usage: onwardpg config check [--config .onwardpg.toml]")
		return 0
	}
	if len(arguments) == 0 || arguments[0] != "check" {
		return writeError("invalid_invocation", errors.New("usage: onwardpg config check [--config .onwardpg.toml]"))
	}
	flags := flag.NewFlagSet("config check", flag.ContinueOnError)
	name := flags.String("config", ".onwardpg.toml", "repository configuration path")
	if help, err := parseFlagSet(flags, arguments[1:]); help {
		return 0
	} else if err != nil {
		return writeError("invalid_invocation", err)
	}
	if code := rejectPositionals(flags, "config check"); code != 0 {
		return code
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
		Name                 string   `json:"name"`
		Provenance           string   `json:"provenance"`
		Fingerprint          string   `json:"fingerprint"`
		DevPostgresMajor     int      `json:"dev_postgres_major"`
		ScratchPostgresMajor int      `json:"scratch_postgres_major"`
		HistoryPostgresMajor int      `json:"history_postgres_major,omitempty"`
		Ignored              []string `json:"ignored,omitempty"`
	}
	checked := make([]checkedTarget, 0, len(targets))
	ctx := context.Background()
	root := filepath.Dir(configPath)
	for _, targetName := range targets {
		target := config.Targets[targetName]
		devURL := os.Getenv(target.DevDatabaseEnv)
		if devURL == "" {
			return writeError("source_error", fmt.Errorf("target %s requires environment variable %s", targetName, target.DevDatabaseEnv))
		}
		adminEnv := target.ScratchEnv()
		adminURL := os.Getenv(adminEnv)
		if adminURL == "" {
			return writeError("source_error", fmt.Errorf("target %s requires environment variable %s", targetName, adminEnv))
		}
		compiled, err := workspace.CompileDDL(ctx, root, targetName, target)
		if err != nil {
			return writeError("source_error", fmt.Errorf("target %s: %w", targetName, err))
		}
		selectors := sortedUniqueStrings(target.Ignore)
		graph, err := source.LoadDDLGraphForComparison(ctx, compiled.DDL, compiled.Provenance, adminURL, selectors)
		if err != nil {
			return writeError("source_error", fmt.Errorf("target %s: %w", targetName, err))
		}
		var ignored []string
		if len(selectors) > 0 {
			development, loadErr := source.LoadGraphForComparison(ctx, source.Parse(devURL), "", selectors)
			if loadErr != nil {
				return writeError("source_error", fmt.Errorf("target %s development catalog: %w", targetName, loadErr))
			}
			if validateErr := source.ValidateIgnoreSelectors(selectors, graph, development); validateErr != nil {
				return writeError("invalid_ignore", fmt.Errorf("target %s: %w", targetName, validateErr))
			}
			ignored = sortedUniqueStrings(append(append([]string(nil), graph.Ignored()...), development.Ignored()...))
		}
		fingerprint, err := graph.Fingerprint()
		if err != nil {
			return writeError("source_error", fmt.Errorf("target %s fingerprint: %w", targetName, err))
		}
		scratchMajor, err := source.PostgresMajor(ctx, adminURL)
		if err != nil {
			return writeError("source_error", fmt.Errorf("target %s: %w", targetName, err))
		}
		devMajor, err := source.PostgresMajor(ctx, devURL)
		if err != nil {
			return writeError("source_error", fmt.Errorf("target %s development database: %w", targetName, err))
		}
		chain, err := history.Load(root, config.BundleRoot, targetName)
		if err != nil {
			return writeError("invalid_history", fmt.Errorf("target %s: %w", targetName, err))
		}
		historyMajor := 0
		for _, entry := range chain.Entries {
			recorded := entry.Artifact.Manifest.DesiredSource.PostgresMajor
			if recorded == 0 {
				continue
			}
			if historyMajor != 0 && historyMajor != recorded {
				return writeError("invalid_history", fmt.Errorf("target %s history mixes PostgreSQL %d and %d receipts", targetName, historyMajor, recorded))
			}
			historyMajor = recorded
		}
		if historyMajor != 0 && (historyMajor != scratchMajor || historyMajor != devMajor) {
			return writeError("incompatible_postgres_major", fmt.Errorf("target %s history requires PostgreSQL %d; development is %d and scratch is %d", targetName, historyMajor, devMajor, scratchMajor))
		}
		checked = append(checked, checkedTarget{
			Name: targetName, Provenance: compiled.Provenance, Fingerprint: fingerprint,
			DevPostgresMajor: devMajor, ScratchPostgresMajor: scratchMajor, HistoryPostgresMajor: historyMajor,
			Ignored: ignored,
		})
	}
	_ = json.NewEncoder(os.Stdout).Encode(struct {
		ProtocolVersion string          `json:"protocol_version"`
		Status          string          `json:"status"`
		ConfigVersion   int             `json:"config_version"`
		Targets         []checkedTarget `json:"targets"`
	}{ProtocolVersion: "onwardpg.config-check/v3", Status: "valid", ConfigVersion: config.Version, Targets: checked})
	return 0
}

func helpRequested(arguments []string) bool {
	return len(arguments) == 1 && (arguments[0] == "help" || arguments[0] == "-h" || arguments[0] == "--help")
}

// parseFlagSet keeps successful help human-readable while ensuring ordinary
// invocation failures emit one machine-readable diagnostic and no flag
// package usage prose on stderr.
func parseFlagSet(flags *flag.FlagSet, arguments []string) (help bool, err error) {
	var output bytes.Buffer
	flags.SetOutput(&output)
	err = flags.Parse(arguments)
	if errors.Is(err, flag.ErrHelp) {
		_, _ = os.Stdout.Write(output.Bytes())
		return true, nil
	}
	return false, err
}

func rejectPositionals(flags *flag.FlagSet, command string) int {
	if flags.NArg() == 0 {
		return 0
	}
	return writeError("invalid_invocation", fmt.Errorf("%s does not accept positional arguments: %s", command, strings.Join(flags.Args(), " ")))
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
			if report.Path != "" {
				if _, err := fmt.Fprintf(writer, "decision receipts written to %s\n", report.Path); err != nil {
					return err
				}
			}
			if report.CreatedBundle {
				if _, err := fmt.Fprintln(writer, "the bundle now exists; omit --create on the next invocation"); err != nil {
					return err
				}
			}
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
			_, err := fmt.Fprintf(writer, "replace every ONWARDPG TODO, then run onwardpg verify --target %s --bundle %s\n", report.Target, report.BundleID)
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
		nextAction := "rerun_same_command_with_hints"
		if report.CreatedBundle {
			nextAction = "rerun_without_create_with_hints"
		}
		return json.NewEncoder(writer).Encode(struct {
			ProtocolVersion string                      `json:"protocol_version"`
			Status          string                      `json:"status"`
			NextAction      string                      `json:"next_action"`
			Path            string                      `json:"path,omitempty"`
			WrittenReceipts []string                    `json:"written_receipts,omitempty"`
			Decisions       []protocol.Decision         `json:"decisions"`
			Analysis        []protocol.DecisionAnalysis `json:"analysis,omitempty"`
		}{
			ProtocolVersion: draftflow.Version, Status: "needs_decisions", NextAction: nextAction,
			Path: report.Path, WrittenReceipts: report.WrittenReceipts, Decisions: report.Decisions,
			Analysis: analysisFromPlan(report.Plan),
		})
	}
	if report.Outcome == string(protocol.NeedsSQLEdits) {
		return json.NewEncoder(writer).Encode(struct {
			ProtocolVersion string   `json:"protocol_version"`
			Status          string   `json:"status"`
			NextAction      string   `json:"next_action"`
			Path            string   `json:"path"`
			Edit            []string `json:"edit"`
		}{ProtocolVersion: draftflow.Version, Status: string(protocol.NeedsSQLEdits), NextAction: "edit_files_then_verify", Path: report.Path, Edit: report.EditFiles})
	}
	return json.NewEncoder(writer).Encode(report)
}

func analysisFromPlan(plan *protocol.Result) []protocol.DecisionAnalysis {
	if plan == nil {
		return nil
	}
	return plan.Analysis
}

func writeDecisionsText(writer io.Writer, subject string, decisions []protocol.Decision) error {
	return writeDecisionsTextWithFlag(writer, subject, "--hint", decisions)
}

func writeDecisionsTextWithFlag(writer io.Writer, subject, flagName string, decisions []protocol.Decision) error {
	if _, err := fmt.Fprintf(writer, "%s needs %d decision(s)\n", subject, len(decisions)); err != nil {
		return err
	}
	for index, decision := range decisions {
		if _, err := fmt.Fprintf(writer, "\nDecision %d: choose exactly one\n", index+1); err != nil {
			return err
		}
		for _, choice := range decision.Choices {
			data, err := json.Marshal(choice.Hint)
			if err != nil {
				return err
			}
			if _, err := fmt.Fprintf(writer, "    %s %s", flagName, shellQuote(string(data))); err != nil {
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
	_, err := fmt.Fprintln(writer, "\nrerun the command with exactly one hint from each decision")
	return err
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'"'"'`) + "'"
}

func writeDecisionEnvelope(writer io.Writer, version string, decisions []protocol.Decision, analysis []protocol.DecisionAnalysis) error {
	return json.NewEncoder(writer).Encode(struct {
		ProtocolVersion string                      `json:"protocol_version"`
		Status          string                      `json:"status"`
		NextAction      string                      `json:"next_action"`
		Decisions       []protocol.Decision         `json:"decisions"`
		Analysis        []protocol.DecisionAnalysis `json:"analysis,omitempty"`
	}{ProtocolVersion: version, Status: "needs_decisions", NextAction: "rerun_same_command_with_hints", Decisions: decisions, Analysis: analysis})
}

func writeError(code string, err error) int {
	if code == "invalid_config" && errors.Is(err, os.ErrNotExist) {
		err = fmt.Errorf("%w; create .onwardpg.toml (see .onwardpg.example.toml), then run onwardpg config check", err)
	}
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

// resolveConfiguredTarget keeps the common one-database workflow free of a
// meaningless target flag while retaining explicit selection for monorepos.
// It runs only after strict configuration validation, so at least one target
// is always present.
func resolveConfiguredTarget(config workspace.Config, selected *string) (workspace.Target, error) {
	if *selected == "" {
		names := make([]string, 0, len(config.Targets))
		for name := range config.Targets {
			names = append(names, name)
		}
		sort.Strings(names)
		if len(names) != 1 {
			return workspace.Target{}, fmt.Errorf("--target is required because this repository configures multiple targets: %s", strings.Join(names, ", "))
		}
		*selected = names[0]
	}
	return config.Target(*selected)
}

func targetIgnoreSelectors(target workspace.Target, command []string) []string {
	return sortedUniqueStrings(append(append([]string(nil), target.Ignore...), command...))
}

type stringsFlag []string

func (s *stringsFlag) String() string { return fmt.Sprint([]string(*s)) }
func (s *stringsFlag) Set(value string) error {
	if value == "" {
		return errors.New("value must not be empty")
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
