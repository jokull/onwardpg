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
	"runtime"
	"runtime/debug"
	"sort"
	"strings"
	"time"

	"github.com/jokull/onwardpg/internal/activeplan"
	"github.com/jokull/onwardpg/internal/bundle"
	"github.com/jokull/onwardpg/internal/contractcheck"
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
var (
	buildVersion = "dev"
	buildCommit  = ""
	buildTime    = ""
)

type buildIdentity struct {
	Version                 string `json:"version"`
	Commit                  string `json:"commit"`
	Dirty                   bool   `json:"dirty"`
	BuildTime               string `json:"build_time,omitempty"`
	GoVersion               string `json:"go_version"`
	SupportedPostgresMajors []int  `json:"supported_postgres_majors"`
}

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

func currentBuildIdentity() buildIdentity {
	identity := buildIdentity{
		Version: currentBuildVersion(), Commit: buildCommit, BuildTime: buildTime,
		GoVersion: runtime.Version(), SupportedPostgresMajors: []int{15, 16, 17, 18},
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, setting := range info.Settings {
			switch setting.Key {
			case "vcs.revision":
				if identity.Commit == "" {
					identity.Commit = setting.Value
				}
			case "vcs.time":
				if identity.BuildTime == "" {
					identity.BuildTime = setting.Value
				}
			case "vcs.modified":
				identity.Dirty = setting.Value == "true"
			}
		}
	}
	if identity.Commit == "" {
		identity.Commit = "unknown"
	}
	return identity
}

func currentBundleBuildIdentity() *bundle.BuildIdentity {
	identity := currentBuildIdentity()
	return &bundle.BuildIdentity{
		Version: identity.Version, Commit: identity.Commit, Dirty: identity.Dirty,
		BuildTime: identity.BuildTime, GoVersion: identity.GoVersion,
		SupportedPostgresMajors: append([]int(nil), identity.SupportedPostgresMajors...),
	}
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
  contract check   check live contract readiness read-only
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
		return writeError("invalid_invocation", errors.New("usage: onwardpg <init|status|history|dev|draft|verify|contract|drift|plan|diff|config|version>"))
	}
	switch os.Args[1] {
	case "help", "-h", "--help":
		_, _ = fmt.Fprintln(os.Stdout, rootUsage)
		return 0
	case "version", "--version":
		if os.Args[1] == "version" && helpRequested(os.Args[2:]) {
			_, _ = fmt.Fprintln(os.Stdout, "Usage: onwardpg version")
			return 0
		}
		if os.Args[1] == "version" && len(os.Args) > 2 {
			return writeError("invalid_invocation", errors.New("version does not accept arguments"))
		}
		_ = json.NewEncoder(os.Stdout).Encode(struct {
			Status string        `json:"status"`
			Build  buildIdentity `json:"build"`
		}{Status: "ok", Build: currentBuildIdentity()})
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
		return runLowLevelPlan("diff", os.Args[2:])
	case "dev":
		return runDev(os.Args[2:])
	case "draft":
		return runDraft(os.Args[2:])
	case "verify":
		return runVerify(os.Args[2:])
	case "contract":
		return runContractAt(os.Args[2:], ".")
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
	arguments = normalizeHelpAlias(arguments)
	flags := flag.NewFlagSet("status", flag.ContinueOnError)
	flags.Usage = func() {
		_, _ = fmt.Fprintln(flags.Output(), "Usage: onwardpg status [options]")
		flags.PrintDefaults()
	}
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
	if !found || !anchor.HasActive() {
		status := "absent"
		if found && len(anchor.Parked) > 0 {
			status = "parked"
		}
		_ = json.NewEncoder(os.Stdout).Encode(struct {
			Status string                 `json:"status"`
			Target string                 `json:"target"`
			Parked []activeplan.SavedPlan `json:"parked,omitempty"`
		}{Status: status, Target: *targetName, Parked: anchor.Parked})
		return 0
	}
	historyStatus, err := history.Inspect(root, config.BundleRoot, *targetName, anchor.BundleID)
	if err != nil {
		_ = json.NewEncoder(os.Stdout).Encode(struct {
			Status   string                  `json:"status"`
			Target   string                  `json:"target"`
			Plan     activeplan.Anchor       `json:"plan"`
			Findings []history.StatusFinding `json:"findings"`
		}{
			Status: "blocked", Target: *targetName, Plan: anchor,
			Findings: []history.StatusFinding{{Code: "invalid_history", Message: err.Error(), Remediation: "restore one replayable accepted chain, then run onwardpg plan"}},
		})
		return 4
	}
	authoringState := "active"
	verificationState := "unverified"
	if historyStatus.Status == "stale" {
		authoringState = "stale_parent"
	} else if historyStatus.Status == "blocked" {
		authoringState = "blocked"
	} else if historyStatus.Status == "missing" {
		authoringState = "absent"
	} else if historyStatus.Selected != nil && historyStatus.Selected.Relationship == "current" {
		destination, pathErr := config.BundlePath(root, *targetName, anchor.BundleID)
		if pathErr == nil {
			artifact, readErr := bundle.Read(destination)
			if readErr == nil && artifact.Manifest.State == string(protocol.Planned) && artifact.Manifest.ExpandCheckpointDigest != "" {
				verificationState = "verified"
				authoringState = "merge_ready"
			}
		}
	}
	_ = json.NewEncoder(os.Stdout).Encode(struct {
		Status       string               `json:"status"`
		Verification string               `json:"verification"`
		Target       string               `json:"target"`
		Plan         activeplan.Anchor    `json:"plan"`
		History      history.StatusReport `json:"history"`
	}{
		Status: authoringState, Verification: verificationState, Target: *targetName, Plan: anchor, History: historyStatus,
	})
	if authoringState == "active" || authoringState == "merge_ready" || authoringState == "absent" {
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
			Status: "blocked", Target: *targetName,
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

func runContractAt(arguments []string, start string) int {
	if helpRequested(arguments) {
		_, _ = fmt.Fprintln(os.Stdout, "Usage: onwardpg contract check [--bundle ID] --environment NAME --database-env ENV [--evidence FILE] [--target NAME]")
		return 0
	}
	if len(arguments) == 0 || arguments[0] != "check" {
		return writeError("invalid_invocation", errors.New("usage: onwardpg contract check [--bundle ID] --environment NAME --database-env ENV [--evidence FILE]"))
	}
	flags := flag.NewFlagSet("contract check", flag.ContinueOnError)
	targetName := flags.String("target", "", "configured database target name")
	bundleID := flags.String("bundle", "", "history-head bundle to check")
	environment := flags.String("environment", "", "deployment environment identity")
	databaseEnv := flags.String("database-env", "", "environment variable containing a read-only PostgreSQL URL")
	evidencePath := flags.String("evidence", "", "writer-drain evidence JSON")
	statementTimeout := flags.Duration("statement-timeout", 30*time.Second, "timeout for each read-only data gate")
	configName := flags.String("config", ".onwardpg.toml", "repository configuration path")
	if help, err := parseFlagSet(flags, arguments[1:]); help {
		return 0
	} else if err != nil {
		return writeError("invalid_invocation", err)
	}
	if code := rejectPositionals(flags, "contract check"); code != 0 {
		return code
	}
	if *environment == "" || *databaseEnv == "" {
		return writeError("invalid_invocation", errors.New("contract check requires --environment and --database-env"))
	}
	databaseURL := os.Getenv(*databaseEnv)
	if databaseURL == "" {
		return writeError("source_error", fmt.Errorf("environment variable %s is required", *databaseEnv))
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
	chain, err := history.Load(filepath.Dir(configPath), config.BundleRoot, *targetName)
	if err != nil {
		return writeError("invalid_history", err)
	}
	if len(chain.Entries) == 0 {
		return writeError("invalid_history", errors.New("target history is empty"))
	}
	entry := chain.Entries[len(chain.Entries)-1]
	if *bundleID != "" && entry.Artifact.Manifest.BundleID != *bundleID {
		return writeError("stale_bundle", fmt.Errorf("bundle %q is not the history head %q", *bundleID, entry.Artifact.Manifest.BundleID))
	}
	var evidence []byte
	if *evidencePath != "" {
		evidence, err = os.ReadFile(*evidencePath)
		if err != nil {
			return writeError("writer_evidence_error", err)
		}
	}
	manifest := entry.Artifact.Manifest
	options := graphplan.Options{
		ConcurrentIndexes: manifest.Planner.Options.ConcurrentIndexes, IfNotExists: manifest.Planner.Options.IfNotExists,
		IfExists: manifest.Planner.Options.IfExists, CascadeDrops: manifest.Planner.Options.CascadeDrops,
		SchemaQualifier:         manifest.Planner.Options.SchemaQualifier,
		IgnoreExtensionVersions: append([]string(nil), manifest.Planner.Options.IgnoreExtensionVersions...),
	}
	report, err := contractcheck.Run(context.Background(), contractcheck.Input{
		Artifact: entry.Artifact, ExpectedHead: chain.HeadDigest, DatabaseURL: databaseURL, Environment: *environment,
		Evidence: evidence, StatementTimeout: *statementTimeout, Ignores: manifest.Planner.IgnoreSelectors, Options: options,
	})
	if err != nil {
		return writeError("contract_check_error", err)
	}
	_ = json.NewEncoder(os.Stdout).Encode(report)
	if report.Status == "ready" {
		return 0
	}
	if report.Status == "needs_evidence" || report.Status == "reconciliation_required" {
		return 2
	}
	return 4
}

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
		return writeError("source_error", fmt.Errorf("environment variable %s is required: drift check replays accepted history into disposable PostgreSQL before comparing that reconstructed catalog with the live database; it never replays history against the live connection", scratchEnv))
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
	var ignoreExtensionVersions stringsFlag
	flags.Var(&ignoreExtensionVersions, "ignore-extension-version", "extension name whose version changes should be ignored; repeat for multiple names")
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
		BuildIdentity:   currentBundleBuildIdentity(),
		Ignores:         selectors,
		RequiredIgnores: sortedUniqueStrings(ignores),
		PlannerOptions: graphplan.Options{
			ConcurrentIndexes:       *concurrentIndexes,
			IgnoreExtensionVersions: sortedUniqueStrings(ignoreExtensionVersions),
		},
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
	var ignoreExtensionVersions stringsFlag
	flags.Var(&ignoreExtensionVersions, "ignore-extension-version", "extension name whose version changes should be ignored; repeat for multiple names")
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
		ConcurrentIndexes:       *concurrentIndexes,
		IfNotExists:             *ifNotExists,
		IfExists:                *ifExists,
		CascadeDrops:            *cascadeDrops,
		IgnoreExtensionVersions: sortedUniqueStrings(ignoreExtensionVersions),
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
			if outputErr == nil {
				outputErr = writeGuidanceText(os.Stdout, result.Guidance)
			}
		} else {
			outputErr = writeDecisionEnvelope(os.Stdout, []string{"onwardpg", "dev", "plan"}, "--hint", report.Decisions, result.Analysis, result.Guidance)
		}
		if outputErr != nil {
			return writeError("output_error", outputErr)
		}
	} else if *output == "text" {
		if result.Status == protocol.Unsupported {
			_ = writeUnsupportedText(os.Stdout, result)
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
				Status             string   `json:"status"`
				Changed            bool     `json:"changed"`
				CurrentFingerprint string   `json:"current_fingerprint"`
				DesiredFingerprint string   `json:"desired_fingerprint"`
				Preserved          []string `json:"preserved,omitempty"`
			}{
				Status: devNoChangeStatus(result), Changed: false,
				CurrentFingerprint: result.CurrentFingerprint, DesiredFingerprint: result.DesiredFingerprint,
				Preserved: result.Preserved,
			})
		} else {
			_ = json.NewEncoder(os.Stdout).Encode(result)
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
	var ignoreExtensionVersions stringsFlag
	flags.Var(&ignoreExtensionVersions, "ignore-extension-version", "extension name whose version changes should be ignored; repeat for multiple names")
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
		ConcurrentIndexes:       *concurrentIndexes,
		IfNotExists:             *ifNotExists,
		IfExists:                *ifExists,
		CascadeDrops:            *cascadeDrops,
		IgnoreExtensionVersions: sortedUniqueStrings(ignoreExtensionVersions),
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
		BuildIdentity:   currentBundleBuildIdentity(),
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
		if !found || !anchor.HasActive() {
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
			message := fmt.Sprintf("strict history validation failed: %v; editable bundle preparation failed: %v", strictErr, prepareErr)
			if strings.Contains(prepareErr.Error(), "ONWARDPG TODO") {
				code = "unresolved_sql_todo"
				message = prepareErr.Error()
				remediation = "replace the remaining TODO at the reported phase line, then run onwardpg verify --target " + *targetName + " --bundle " + *bundleID
			}
			return writeVerifyFinding(*targetName, *bundleID, "", *through, "blocked", code,
				message, remediation)
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
			Outcome:      "blocked",
			Target:       *targetName,
			BundleID:     *bundleID,
			HistoryHead:  chain.HeadDigest,
			ThroughPhase: *through,
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
	if *check && manifest.ContractGatesDigest != "" && manifest.ExpandCheckpointDigest == "" {
		return writeVerifyFinding(*targetName, *bundleID, prefix.HeadDigest, *through, "blocked", "missing_expand_checkpoint",
			"the gated contract bundle has no receipted post-expand catalog checkpoint",
			"rerun onwardpg plan or verify the edited bundle so disposable PostgreSQL can receipt its exact expand graph")
	}
	if selectedAnchor != nil && manifest.PlanID != selectedAnchor.PlanID {
		return writeVerifyFinding(*targetName, *bundleID, prefix.HeadDigest, *through, "blocked", "active_plan_identity_mismatch",
			fmt.Sprintf("local active plan %s does not match bundle plan id %s", selectedAnchor.PlanID, manifest.PlanID),
			"select the intended bundle explicitly or remove the stale local active-plan anchor")
	}
	options := graphplan.Options{
		ConcurrentIndexes:       manifest.Planner.Options.ConcurrentIndexes,
		IfNotExists:             manifest.Planner.Options.IfNotExists,
		IfExists:                manifest.Planner.Options.IfExists,
		CascadeDrops:            manifest.Planner.Options.CascadeDrops,
		SchemaQualifier:         manifest.Planner.Options.SchemaQualifier,
		IgnoreExtensionVersions: append([]string(nil), manifest.Planner.Options.IgnoreExtensionVersions...),
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
	workingFingerprint, err := graphplan.Fingerprint(working, options)
	if err != nil {
		return writeError("source_error", fmt.Errorf("fingerprint current desired schema: %w", err))
	}
	if workingFingerprint != manifest.DesiredSource.Fingerprint {
		_ = json.NewEncoder(os.Stdout).Encode(verify.Report{
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
	verificationChain := chain
	var checkpointArtifact *bundle.Artifact
	var report verify.Report
	if edited != nil {
		expandReport, expandErr := verify.Run(ctx, verify.Input{
			AdminURL: adminURL, Chain: chain, BundleID: *bundleID, ThroughPhase: protocol.PhaseExpand,
			Ignores: manifest.Planner.IgnoreSelectors, Options: options,
		})
		if expandErr != nil {
			return writeError("verification_error", expandErr)
		}
		if expandReport.Outcome != "verified" && expandReport.Outcome != "partial_verified" {
			_ = json.NewEncoder(os.Stdout).Encode(expandReport)
			return 4
		}
		receipted, receiptErr := bundle.WithExpandCheckpoint(edited.Artifact, expandReport.ObservedFingerprint)
		if receiptErr != nil {
			return writeError("bundle_receipt_update_failed", receiptErr)
		}
		checkpointArtifact = &receipted
		verificationChain.Entries = append([]history.Entry(nil), chain.Entries...)
		verificationChain.Entries[len(verificationChain.Entries)-1].Artifact = receipted
		verificationChain.HeadDigest = receipted.Manifest.History.EntryDigest
		if *through == protocol.PhaseExpand {
			report = expandReport
			report.HistoryHead = verificationChain.HeadDigest
		} else {
			report, err = verify.Run(ctx, verify.Input{
				AdminURL: adminURL, Chain: verificationChain, BundleID: *bundleID, ThroughPhase: *through,
				Ignores: manifest.Planner.IgnoreSelectors, Options: options,
			})
		}
	} else {
		report, err = verify.Run(ctx, verify.Input{
			AdminURL: adminURL, Chain: chain, BundleID: *bundleID, ThroughPhase: *through,
			Ignores: manifest.Planner.IgnoreSelectors, Options: options,
		})
	}
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
			if loadErr != nil || latestSelected == nil || latestBase.HeadDigest != edited.Artifact.Manifest.History.ParentDigest || !reflect.DeepEqual(latestSelected.Artifact.Files, edited.Artifact.Files) {
				message := "target history or edited SQL changed while clone verification was running"
				if loadErr != nil {
					message += ": " + loadErr.Error()
				}
				return writeVerifyFinding(*targetName, *bundleID, chain.HeadDigest, *through, "blocked", "history_changed_during_verify",
					message, "inspect the edited bundle and rerun verification; no receipts were installed")
			}
		}
	}
	if (report.Outcome == "verified" || report.Outcome == "partial_verified") && checkpointArtifact != nil {
		destination, err := config.BundlePath(root, *targetName, *bundleID)
		if err != nil {
			return writeError("invalid_bundle", err)
		}
		if err := bundle.InstallReceipts(destination, *checkpointArtifact); err != nil {
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
		Outcome:      outcome,
		Target:       target,
		BundleID:     bundleID,
		HistoryHead:  historyHead,
		ThroughPhase: through,
		Findings: []verify.Finding{{
			Code: code, Message: message, Remediation: remediation,
		}},
	})
	return 4
}

// runPlan is the ergonomic planning command. The old explicit --from/--to
// spelling remains accepted here for one preview transition and is available
// permanently as `onwardpg diff`.
func runPlan(arguments []string) int {
	for _, argument := range arguments {
		if argument == "--from" || argument == "--to" || strings.HasPrefix(argument, "--from=") || strings.HasPrefix(argument, "--to=") {
			return runLowLevelPlan("plan", arguments)
		}
	}
	return runWorkflowPlanAt(arguments, ".")
}

func runWorkflowPlanAt(arguments []string, start string) int {
	arguments = normalizeHelpAlias(arguments)
	name := ""
	if len(arguments) > 0 && !strings.HasPrefix(arguments[0], "-") {
		name = arguments[0]
		arguments = arguments[1:]
	}
	flags := flag.NewFlagSet("plan", flag.ContinueOnError)
	flags.Usage = func() {
		_, _ = fmt.Fprintln(flags.Output(), "Usage: onwardpg plan [NAME] [options]")
		flags.PrintDefaults()
	}
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
	var ignoreExtensionVersions stringsFlag
	flags.Var(&ignoreExtensionVersions, "ignore-extension-version", "extension name whose version changes should be ignored; repeat for multiple names")
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
	adminURL := os.Getenv(target.ScratchEnv())
	hints, err := readHints(inlineHints, *hintsFile)
	if err != nil {
		return writeError("invalid_hints", err)
	}
	devHints, err := readHints(inlineDevHints, *devHintsFile)
	if err != nil {
		return writeError("invalid_development_hints", err)
	}
	options := graphplan.Options{
		ConcurrentIndexes:       *concurrentIndexes,
		IfNotExists:             *ifNotExists,
		IfExists:                *ifExists,
		CascadeDrops:            *cascadeDrops,
		IgnoreExtensionVersions: sortedUniqueStrings(ignoreExtensionVersions),
	}
	if schemaQualifier.set {
		options.SchemaQualifier = &schemaQualifier.value
	}
	anchor, stored, err := activeplan.Load(root, *targetName)
	if err != nil {
		return writeError("active_plan_error", err)
	}
	anchored := stored && anchor.HasActive()
	var planID string
	createBundle := false
	parked := []activeplan.SavedPlan(nil)
	if stored {
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
			if saved, exists := anchor.FindParked(name); exists {
				planID = saved.PlanID
				parked = anchor.WithActive(saved).Parked
			} else {
				planID, err = activeplan.NewID()
				if err != nil {
					return writeError("active_plan_error", err)
				}
				createBundle = true
			}
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
			if stored {
				parked = anchor.WithActive(activeplan.SavedPlan{PlanID: planID, BundleID: *bundleID}).Parked
			}
		}
	case anchored:
		*bundleID, planID = anchor.BundleID, anchor.PlanID
	default:
		return writeWorkflowPlanFinding(*targetName, "", "blocked", "active_plan_required",
			"this workspace has no active plan", "start one with onwardpg plan NAME")
	}
	if adminURL == "" {
		return writeError("source_error", fmt.Errorf("environment variable %s is required", target.ScratchEnv()))
	}
	report, err := draftflow.Run(context.Background(), draftflow.Input{
		Root: root, ConfigPath: configPath, Config: config, TargetName: *targetName, Target: target,
		AdminURL: adminURL, BundleID: *bundleID, PlanID: planID, InferBase: true,
		Create: createBundle, BuildVersion: currentBuildVersion(), BuildIdentity: currentBundleBuildIdentity(), Purpose: *purpose,
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
		if err := storeInactivePlan(root, anchor.WithoutActive()); err != nil {
			return writeError("active_plan_error", err)
		}
	} else if report.Path != "" {
		if err := activeplan.Store(root, activeplan.Anchor{Version: activeplan.Version, Target: *targetName, PlanID: planID, BundleID: *bundleID, Parked: parked}); err != nil {
			return writeError("active_plan_error", err)
		}
	}
	devURL := os.Getenv(target.DevDatabaseEnv)
	if devURL == "" {
		if len(inlineDevHints) > 0 || *devHintsFile != "" {
			return writeError("invalid_development_hints", fmt.Errorf("environment variable %s is required when supplying development hints", target.DevDatabaseEnv))
		}
		development := devflow.Report{Status: protocol.Status("not_available")}
		if err := writeWorkflowPlanReport(os.Stdout, *output, report, development); err != nil {
			return writeError("output_error", err)
		}
		return workflowPlanExitCode(report.Outcome, development)
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

func storeInactivePlan(root string, anchor activeplan.Anchor) error {
	if len(anchor.Parked) == 0 {
		return activeplan.Clear(root, anchor.Target)
	}
	return activeplan.Store(root, anchor)
}

func developmentPostconditions(checks []draftflow.DevelopmentPostcondition) []devflow.Postcondition {
	result := make([]devflow.Postcondition, 0, len(checks))
	for _, check := range checks {
		result = append(result, devflow.Postcondition{BundleID: check.BundleID, Path: check.Path, ID: check.ID, SQL: check.SQL})
	}
	return result
}

type workflowPlanReport struct {
	Status      string               `json:"status"`
	Durable     draftflow.Report     `json:"durable"`
	Development devflow.Report       `json:"development"`
	NextActions []workflowNextAction `json:"next_actions,omitempty"`
}

type workflowNextAction struct {
	Scope          string                 `json:"scope"`
	Kind           string                 `json:"kind"`
	Reason         string                 `json:"reason,omitempty"`
	Argv           []string               `json:"argv,omitempty"`
	SQL            string                 `json:"sql,omitempty"`
	StatementCount int                    `json:"statement_count,omitempty"`
	Preserved      []string               `json:"preserved,omitempty"`
	Choices        []workflowActionChoice `json:"choices,omitempty"`
	Path           string                 `json:"path,omitempty"`
	JSONPointer    string                 `json:"json_pointer,omitempty"`
	PocketID       string                 `json:"pocket_id,omitempty"`
	Purpose        string                 `json:"purpose,omitempty"`
	Phase          string                 `json:"phase,omitempty"`
	ExecutionMode  string                 `json:"execution_mode,omitempty"`
	RequiredProof  string                 `json:"required_proof,omitempty"`
}

type workflowActionChoice struct {
	Hint    protocol.Hint `json:"hint"`
	Argv    []string      `json:"argv"`
	Hazards []string      `json:"hazards,omitempty"`
}

func newWorkflowPlanReport(durable draftflow.Report, development devflow.Report) workflowPlanReport {
	return workflowPlanReport{
		Status:      workflowPlanStatus(durable.Outcome, development),
		Durable:     durable,
		Development: development,
		NextActions: workflowNextActions(durable, development),
	}
}

func workflowNextActions(durable draftflow.Report, development devflow.Report) []workflowNextAction {
	var result []workflowNextAction
	appendDecisions := func(scope, flag string, carried []protocol.Hint, decisions []protocol.Decision) {
		for _, decision := range decisions {
			action := workflowNextAction{Scope: scope, Kind: "semantic_hint"}
			for _, choice := range decision.Choices {
				argv := []string{"onwardpg", "plan"}
				valid := true
				for _, hint := range append(append([]protocol.Hint(nil), carried...), choice.Hint) {
					encoded, err := json.Marshal(hint)
					if err != nil {
						valid = false
						break
					}
					argv = append(argv, flag, string(encoded))
				}
				if !valid {
					continue
				}
				action.Choices = append(action.Choices, workflowActionChoice{
					Hint: choice.Hint, Argv: argv,
					Hazards: append([]string(nil), choice.Hazards...),
				})
			}
			if len(action.Choices) > 0 {
				result = append(result, action)
			}
		}
	}
	appendDecisions("durable", "--hint", nil, durable.Decisions)
	for _, hint := range durable.DeferredHints {
		encoded, err := json.Marshal(hint)
		if err != nil {
			continue
		}
		result = append(result, workflowNextAction{
			Scope: "durable", Kind: "deferred_hint",
			Choices: []workflowActionChoice{{Hint: hint, Argv: []string{"onwardpg", "plan", "--hint", string(encoded)}}},
		})
	}
	if durable.Outcome == string(protocol.NeedsSQLEdits) {
		for _, edit := range durable.EditRequirements {
			result = append(result, workflowNextAction{
				Scope: "durable", Kind: "edit_file", Path: filepath.ToSlash(filepath.Join(durable.Path, edit.Path)),
				PocketID: edit.PocketID, Purpose: edit.Purpose, Phase: edit.Phase,
				ExecutionMode: edit.ExecutionMode, RequiredProof: edit.RequiredProof,
			})
		}
		if len(durable.EditRequirements) == 0 {
			for _, path := range durable.EditFiles {
				result = append(result, workflowNextAction{Scope: "durable", Kind: "edit_file", Path: filepath.ToSlash(filepath.Join(durable.Path, path))})
			}
		}
	}
	appendDecisions("development", "--dev-hint", development.AppliedHints, development.Decisions)
	for _, hint := range development.DeferredHints {
		argv := []string{"onwardpg", "plan"}
		for _, carried := range append(append([]protocol.Hint(nil), development.AppliedHints...), hint) {
			encoded, err := json.Marshal(carried)
			if err != nil {
				continue
			}
			argv = append(argv, "--dev-hint", string(encoded))
		}
		result = append(result, workflowNextAction{
			Scope: "development", Kind: "deferred_hint",
			Choices: []workflowActionChoice{{Hint: hint, Argv: argv}},
		})
	}
	for index, check := range development.Postconditions {
		if check.Status != "passed" {
			result = append(result, workflowNextAction{
				Scope: "development", Kind: "review_postcondition",
				JSONPointer: fmt.Sprintf("/development/accepted_postconditions/%d", index),
			})
		}
	}
	if durablePlanReady(durable.Outcome) && development.Status == protocol.Planned && len(development.Result.Statements) > 0 {
		reason := "development_database_behind_desired_schema"
		if durable.ParentChanged {
			reason = "accepted_history_changed"
		}
		argv := []string{"onwardpg", "plan"}
		// A no-change plan has no durable bundle to keep active, and an
		// absorbed generated bundle has just been removed. Naming the plan in
		// those two cases makes this emitted replay command self-contained:
		// it may rebuild the same empty H -> W comparison while rendering the
		// still-useful D -> W reconciliation.
		if (durable.Outcome == "no_changes" && durable.Path == "") || durable.RemovedBundle {
			argv = append(argv, durable.BundleID)
		}
		for _, hint := range development.AppliedHints {
			encoded, err := json.Marshal(hint)
			if err != nil {
				continue
			}
			argv = append(argv, "--dev-hint", string(encoded))
		}
		argv = append(argv, "--output", "sql")
		result = append(result, workflowNextAction{
			Scope: "development", Kind: "workspace_fast_forward", Reason: reason,
			Argv: argv,
			SQL:  protocol.RenderSQL(development.Result, ""), StatementCount: len(development.Result.Statements),
			Preserved: append([]string(nil), development.Result.Preserved...),
		})
	}
	return result
}

func durablePlanReady(outcome string) bool {
	return outcome == string(protocol.Planned) || outcome == "no_changes" || outcome == "absorbed"
}

func writeWorkflowPlanReport(writer io.Writer, output string, durable draftflow.Report, development devflow.Report) error {
	if output == "sql" {
		if development.Status == protocol.Status("not_available") {
			_, err := fmt.Fprintln(writer, "-- onwardpg: durable plan stored; development database is not available, so no workspace reconciliation was attempted")
			return err
		}
		durableReady := durable.Outcome == string(protocol.Planned) || durable.Outcome == "no_changes" || durable.Outcome == "absorbed"
		if durableReady && development.Status == protocol.Planned {
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
		return json.NewEncoder(os.Stderr).Encode(newWorkflowPlanReport(durable, development))
	}
	if output == "text" {
		envelope := newWorkflowPlanReport(durable, development)
		if _, err := fmt.Fprintf(writer, "status: %s\ndurable: %s\ndevelopment: %s\n", envelope.Status, durable.Outcome, development.Status); err != nil {
			return err
		}
		if durable.Path != "" {
			_, _ = fmt.Fprintln(writer, "bundle: "+durable.Path)
		}
		if len(envelope.NextActions) > 0 {
			_, _ = fmt.Fprintln(writer, "next actions:")
			for _, action := range envelope.NextActions {
				switch action.Kind {
				case "semantic_hint", "deferred_hint":
					for _, choice := range action.Choices {
						_, _ = fmt.Fprintf(writer, "  %s %s: %s\n", action.Scope, action.Kind, renderArgv(choice.Argv))
					}
				case "edit_file":
					_, _ = fmt.Fprintf(writer, "  durable edit: %s (%s; proof: %s)\n", action.Path, action.Purpose, action.RequiredProof)
				case "review_postcondition":
					_, _ = fmt.Fprintf(writer, "  development review: %s\n", action.JSONPointer)
				case "workspace_fast_forward":
					_, _ = fmt.Fprintf(writer, "  development fast-forward: %s (%d statement(s); %s)\n", renderArgv(action.Argv), action.StatementCount, action.Reason)
				}
			}
		}
		if development.Status == protocol.Planned && len(development.Result.Statements) > 0 {
			_, _ = fmt.Fprintln(writer, "\ndevelopment workspace SQL:")
			if _, err := fmt.Fprintln(writer, protocol.RenderSQL(development.Result, "")); err != nil {
				return err
			}
		}
		for _, finding := range durable.Findings {
			_, _ = fmt.Fprintf(writer, "finding %s: %s\n", finding.Code, finding.Message)
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
	return json.NewEncoder(writer).Encode(newWorkflowPlanReport(durable, development))
}

func renderArgv(argv []string) string {
	parts := make([]string, len(argv))
	for index, argument := range argv {
		parts[index] = shellQuote(argument)
	}
	return strings.Join(parts, " ")
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
	switch durable {
	case "stale":
		return "stale"
	case "blocked", string(protocol.Unsupported):
		return "blocked"
	case string(protocol.NeedsInput), string(protocol.NeedsSQLEdits):
		return "needs_action"
	case string(protocol.Planned), "no_changes", "absorbed":
	default:
		return "blocked"
	}
	if development.Status == protocol.Status("not_available") {
		return "ready"
	}
	if development.Status == protocol.NeedsInput {
		return "needs_action"
	}
	if development.Status == protocol.Unsupported {
		return "blocked"
	}
	if failedDevelopmentPostconditions(development.Postconditions) {
		return "needs_action"
	}
	return "ready"
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
		if development.Status == protocol.Status("not_available") {
			return 0
		}
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
		Status   string              `json:"status"`
		Target   string              `json:"target"`
		BundleID string              `json:"bundle_id,omitempty"`
		Findings []draftflow.Finding `json:"findings"`
	}{
		Status: outcome, Target: target, BundleID: bundleID,
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

func runLowLevelPlan(command string, arguments []string) int {
	arguments = normalizeHelpAlias(arguments)
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	flags.Usage = func() {
		_, _ = fmt.Fprintf(flags.Output(), "Usage: onwardpg %s --from SOURCE --to SOURCE [options]\n", command)
		flags.PrintDefaults()
	}
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
	var ignoreExtensionVersions stringsFlag
	flags.Var(&ignoreExtensionVersions, "ignore-extension-version", "extension name whose version changes should be ignored; repeat for multiple names")
	if help, err := parseFlagSet(flags, arguments); help {
		return 0
	} else if err != nil {
		return writeError("invalid_invocation", err)
	}
	if code := rejectPositionals(flags, command); code != 0 {
		return code
	}
	if *from == "" || *to == "" {
		return writeError("invalid_invocation", fmt.Errorf("%s requires --from and --to", command))
	}
	if *output != "json" && *output != "text" {
		return writeError("invalid_invocation", fmt.Errorf("%s --output must be text or json", command))
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
	options := graphplan.Options{
		ConcurrentIndexes:       *concurrentIndexes,
		IfNotExists:             *ifNotExists,
		IfExists:                *ifExists,
		CascadeDrops:            *cascadeDrops,
		IgnoreExtensionVersions: sortedUniqueStrings(ignoreExtensionVersions),
	}
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
			outputErr = writeDecisionsText(os.Stdout, command, decisions)
			if outputErr == nil {
				outputErr = writeGuidanceText(os.Stdout, result.Guidance)
			}
		} else {
			outputErr = writeDecisionEnvelope(os.Stdout, []string{"onwardpg", command}, "--hint", decisions, result.Analysis, result.Guidance)
		}
		if outputErr != nil {
			return writeError("output_error", outputErr)
		}
	} else if *output == "text" {
		if result.Status == protocol.Unsupported {
			_ = writeUnsupportedText(os.Stdout, result)
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
		Status        string          `json:"status"`
		ConfigVersion int             `json:"config_version"`
		Targets       []checkedTarget `json:"targets"`
	}{Status: "valid", ConfigVersion: config.Version, Targets: checked})
	return 0
}

func helpRequested(arguments []string) bool {
	return len(arguments) == 1 && (arguments[0] == "help" || arguments[0] == "-h" || arguments[0] == "--help")
}

func normalizeHelpAlias(arguments []string) []string {
	if len(arguments) == 1 && arguments[0] == "help" {
		return []string{"--help"}
	}
	return arguments
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
			if err := writeDecisionsText(writer, report.BundleID, report.Decisions); err != nil {
				return err
			}
			if report.Plan != nil {
				return writeGuidanceText(writer, report.Plan.Guidance)
			}
			return nil
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
			Status          string                      `json:"status"`
			NextAction      string                      `json:"next_action"`
			Path            string                      `json:"path,omitempty"`
			WrittenReceipts []string                    `json:"written_receipts,omitempty"`
			Decisions       []protocol.Decision         `json:"decisions"`
			Analysis        []protocol.DecisionAnalysis `json:"analysis,omitempty"`
			Guidance        []protocol.Guidance         `json:"guidance,omitempty"`
		}{
			Status: "needs_decisions", NextAction: nextAction,
			Path: report.Path, WrittenReceipts: report.WrittenReceipts,
			Decisions: decisionsWithArgv(report.Decisions, []string{"onwardpg", "draft"}, "--hint"),
			Analysis:  analysisFromPlan(report.Plan), Guidance: guidanceFromPlan(report.Plan),
		})
	}
	if report.Outcome == string(protocol.NeedsSQLEdits) {
		return json.NewEncoder(writer).Encode(struct {
			Status     string   `json:"status"`
			NextAction string   `json:"next_action"`
			Path       string   `json:"path"`
			Edit       []string `json:"edit"`
		}{Status: string(protocol.NeedsSQLEdits), NextAction: "edit_files_then_verify", Path: report.Path, Edit: report.EditFiles})
	}
	return json.NewEncoder(writer).Encode(report)
}

func analysisFromPlan(plan *protocol.Result) []protocol.DecisionAnalysis {
	if plan == nil {
		return nil
	}
	return plan.Analysis
}

func guidanceFromPlan(plan *protocol.Result) []protocol.Guidance {
	if plan == nil {
		return nil
	}
	return plan.Guidance
}

func writeUnsupportedText(writer io.Writer, result protocol.Result) error {
	if _, err := fmt.Fprintln(writer, "unsupported"); err != nil {
		return err
	}
	for _, reason := range result.Unsupported {
		if _, err := fmt.Fprintln(writer, "  "+reason); err != nil {
			return err
		}
	}
	return writeGuidanceText(writer, result.Guidance)
}

func writeGuidanceText(writer io.Writer, guidanceItems []protocol.Guidance) error {
	for _, guidance := range guidanceItems {
		if _, err := fmt.Fprintf(writer, "\n%s guidance for %s\n", guidance.Kind, guidance.Key); err != nil {
			return err
		}
		if _, err := fmt.Fprintln(writer, guidance.Summary); err != nil {
			return err
		}
		for _, step := range guidance.Steps {
			if _, err := fmt.Fprintf(writer, "\n-- %s\n%s\n", step.Stage, step.SQL); err != nil {
				return err
			}
		}
	}
	return nil
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

func writeDecisionEnvelope(writer io.Writer, command []string, flagName string, decisions []protocol.Decision, analysis []protocol.DecisionAnalysis, guidance []protocol.Guidance) error {
	return json.NewEncoder(writer).Encode(struct {
		Status     string                      `json:"status"`
		NextAction string                      `json:"next_action"`
		Decisions  []protocol.Decision         `json:"decisions"`
		Analysis   []protocol.DecisionAnalysis `json:"analysis,omitempty"`
		Guidance   []protocol.Guidance         `json:"guidance,omitempty"`
	}{Status: "needs_decisions", NextAction: "rerun_same_command_with_hints", Decisions: decisionsWithArgv(decisions, command, flagName), Analysis: analysis, Guidance: guidance})
}

func decisionsWithArgv(decisions []protocol.Decision, command []string, flagName string) []protocol.Decision {
	result := make([]protocol.Decision, len(decisions))
	for decisionIndex, decision := range decisions {
		result[decisionIndex].Choices = make([]protocol.DecisionChoice, len(decision.Choices))
		for choiceIndex, choice := range decision.Choices {
			choice.Hazards = append([]string(nil), choice.Hazards...)
			encoded, err := json.Marshal(choice.Hint)
			if err == nil {
				choice.Argv = append(append(append([]string(nil), command...), flagName), string(encoded))
			}
			result[decisionIndex].Choices[choiceIndex] = choice
		}
	}
	return result
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
