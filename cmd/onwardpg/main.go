package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jokull/onwardpg/internal/bundle"
	"github.com/jokull/onwardpg/internal/freshness"
	"github.com/jokull/onwardpg/internal/gitbase"
	"github.com/jokull/onwardpg/internal/graphplan"
	"github.com/jokull/onwardpg/internal/history"
	"github.com/jokull/onwardpg/internal/historyinit"
	"github.com/jokull/onwardpg/internal/prflow"
	"github.com/jokull/onwardpg/internal/protocol"
	"github.com/jokull/onwardpg/internal/source"
	"github.com/jokull/onwardpg/internal/verify"
	"github.com/jokull/onwardpg/internal/workspace"
)

var buildVersion = "dev"

func main() { os.Exit(run()) }

func run() int {
	if len(os.Args) < 2 {
		return writeError("invalid_invocation", errors.New("usage: onwardpg <plan|dev|history|bundle|config|pr|ci>"))
	}
	switch os.Args[1] {
	case "plan":
		return runPlan(os.Args[2:])
	case "dev":
		return runDev(os.Args[2:])
	case "history":
		return runHistory(os.Args[2:])
	case "config":
		return runConfig(os.Args[2:])
	case "bundle":
		return runBundle(os.Args[2:])
	case "ci":
		return runCI(os.Args[2:])
	case "pr":
		return runPR(os.Args[2:])
	default:
		return writeError("invalid_invocation", fmt.Errorf("unknown command %q", os.Args[1]))
	}
}

func runHistory(arguments []string) int { return runHistoryAt(arguments, ".") }

func runHistoryAt(arguments []string, start string) int {
	if len(arguments) == 0 || arguments[0] != "init" {
		return writeError("invalid_invocation", errors.New("usage: onwardpg history init --target NAME [--bundle baseline]"))
	}
	flags := flag.NewFlagSet("history init", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	targetName := flags.String("target", "", "configured database target name")
	bundleID := flags.String("bundle", "baseline", "root history bundle identifier")
	configName := flags.String("config", ".onwardpg.toml", "repository configuration path")
	answerFile := flags.String("answers", "", "fingerprint-bound planner answers")
	concurrentIndexes := flags.Bool("concurrent-indexes", false, "create standalone indexes concurrently")
	var ignores stringsFlag
	flags.Var(&ignores, "ignore", "validated catalog selector to exclude")
	if err := flags.Parse(arguments[1:]); err != nil {
		return writeError("invalid_invocation", err)
	}
	if *targetName == "" {
		return writeError("invalid_invocation", errors.New("history init requires --target"))
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
	adminURL := os.Getenv(target.DevDatabaseEnv)
	if adminURL == "" {
		return writeError("source_error", fmt.Errorf("environment variable %s is required", target.DevDatabaseEnv))
	}
	answers, err := readAnswers(*answerFile)
	if err != nil {
		return writeError("invalid_answers", err)
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
		Answers:        answers,
		AnswersGiven:   *answerFile != "",
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
		return writeError("invalid_invocation", errors.New("usage: onwardpg dev plan --target NAME [--answers FILE] [--sql]"))
	}
	flags := flag.NewFlagSet("dev plan", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	targetName := flags.String("target", "", "configured database target name")
	configName := flags.String("config", ".onwardpg.toml", "repository configuration path")
	answerFile := flags.String("answers", "", "fingerprint-bound planner answers")
	sqlOutput := flags.Bool("sql", false, "print planned SQL instead of JSON")
	indent := flags.String("indent", "", "prefix each rendered SQL line")
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
	answers, err := readAnswers(*answerFile)
	if err != nil {
		return writeError("invalid_answers", err)
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
	desired, err := source.LoadDDLGraphForComparison(ctx, compiled.DDL, compiled.Provenance, devURL, ignores)
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
	result, err := graphplan.Build(current, desired, answers, options)
	if err != nil {
		return writeError("planning_error", err)
	}
	if *sqlOutput && result.Status == protocol.Planned {
		_, _ = fmt.Fprintln(os.Stdout, protocol.RenderSQL(result, *indent))
	} else {
		_ = json.NewEncoder(os.Stdout).Encode(result)
	}
	return resultExitCode(result.Status)
}

func runBundle(arguments []string) int {
	return runBundleAt(arguments, ".")
}

func runBundleAt(arguments []string, start string) int {
	if len(arguments) == 0 || arguments[0] != "verify" {
		return writeError("invalid_invocation", errors.New("usage: onwardpg bundle verify --target NAME --bundle ID [--through PHASE]"))
	}
	flags := flag.NewFlagSet("bundle verify", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	targetName := flags.String("target", "", "configured database target name")
	bundleID := flags.String("bundle", "", "bundle to verify")
	through := flags.String("through", "contract", "last phase to execute: expand, migrate, manual, or contract")
	configName := flags.String("config", ".onwardpg.toml", "repository configuration path")
	if err := flags.Parse(arguments[1:]); err != nil {
		return writeError("invalid_invocation", err)
	}
	if *targetName == "" || *bundleID == "" {
		return writeError("invalid_invocation", errors.New("bundle verify requires --target and --bundle"))
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
	adminURL := os.Getenv(target.DevDatabaseEnv)
	if adminURL == "" {
		return writeError("source_error", fmt.Errorf("environment variable %s is required", target.DevDatabaseEnv))
	}
	chain, err := history.Load(filepath.Dir(configPath), config.BundleRoot, *targetName)
	if err != nil {
		return writeError("invalid_history", err)
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
	report, err := verify.Run(context.Background(), verify.Input{
		AdminURL: adminURL, Chain: chain, BundleID: *bundleID, ThroughPhase: *through,
		Ignores: manifest.Planner.IgnoreSelectors, Options: options,
	})
	if err != nil {
		return writeError("verification_error", err)
	}
	_ = json.NewEncoder(os.Stdout).Encode(report)
	if report.Outcome == "verified" {
		return 0
	}
	return 4
}

const ciVersion = "onwardpg.ci-check/v1"

type ciFinding struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type ciReport struct {
	ProtocolVersion string          `json:"protocol_version"`
	Outcome         string          `json:"outcome"`
	Target          string          `json:"target"`
	BundleID        string          `json:"bundle_id"`
	Repository      gitbase.Status  `json:"repository"`
	PRStatus        *prStatusReport `json:"pr_status,omitempty"`
	Verification    *verify.Report  `json:"verification,omitempty"`
	Findings        []ciFinding     `json:"findings,omitempty"`
}

func runCI(arguments []string) int { return runCIAt(arguments, ".") }

func runCIAt(arguments []string, start string) int {
	if len(arguments) == 0 || arguments[0] != "check" {
		return writeError("invalid_invocation", errors.New("usage: onwardpg ci check --base REF --target NAME --bundle ID"))
	}
	flags := flag.NewFlagSet("ci check", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	base := flags.String("base", "", "exact PR base ref")
	targetName := flags.String("target", "", "configured database target name")
	bundleID := flags.String("bundle", "", "single logical bundle owned by this PR")
	configName := flags.String("config", ".onwardpg.toml", "repository configuration path")
	if err := flags.Parse(arguments[1:]); err != nil {
		return writeError("invalid_invocation", err)
	}
	if *base == "" || *targetName == "" || *bundleID == "" {
		return writeError("invalid_invocation", errors.New("ci check requires --base, --target, and --bundle"))
	}
	ctx := context.Background()
	repository, err := gitbase.Open(ctx, start)
	if err != nil {
		return writeError("git_status_error", err)
	}
	configPath := *configName
	if !filepath.IsAbs(configPath) {
		configPath = filepath.Join(repository.Root, configPath)
	}
	config, err := workspace.Load(configPath)
	if err != nil {
		return writeError("invalid_config", err)
	}
	target, err := config.Target(*targetName)
	if err != nil {
		return writeError("invalid_config", err)
	}
	historyPath := filepath.ToSlash(filepath.Join(config.BundleRoot, *targetName))
	status, err := repository.Inspect(ctx, gitbase.Options{
		BaseRef: *base, HeadRef: "HEAD", HistoryPath: historyPath,
		IncludeWorkingTree: true, ExcludePaths: []string{config.BundleRoot},
	})
	if err != nil {
		return writeError("git_status_error", err)
	}
	report := ciReport{
		ProtocolVersion: ciVersion, Outcome: "failed", Target: *targetName,
		BundleID: *bundleID, Repository: status,
	}
	add := func(code, message string) {
		report.Findings = append(report.Findings, ciFinding{Code: code, Message: message})
	}
	for _, problem := range status.Problems {
		add(problem.Code, problem.Message)
	}
	if status.Dirty {
		add("dirty_working_tree", "CI verification requires committed schema and configuration inputs")
	}
	ids, hasWorkingHistory := branchBundleIDs(status)
	if hasWorkingHistory {
		add("uncommitted_bundle", "the logical migration bundle must be committed before CI verification")
	}
	if len(ids) != 1 || ids[0] != *bundleID {
		add("incorrect_bundle_stack", fmt.Sprintf("PR must own exactly bundle %q for target %q; found %v", *bundleID, *targetName, ids))
	}
	if len(report.Findings) > 0 {
		_ = json.NewEncoder(os.Stdout).Encode(report)
		return 4
	}
	prStatus, err := assessPRBundle(ctx, repository, config, *targetName, *bundleID, target, status)
	if err != nil {
		var conflict *gitbase.MergeConflictError
		if errors.As(err, &conflict) {
			add("merge_conflict", conflict.Error())
			_ = json.NewEncoder(os.Stdout).Encode(report)
			return 4
		}
		return writeError("pr_analysis_error", err)
	}
	report.PRStatus = &prStatus
	if prStatus.Outcome != "fresh" || prStatus.Analysis == nil || prStatus.Analysis.Outcome != "ready" {
		add("pr_bundle_not_fresh", "the bundle is stale, unanswered, unsupported, or does not converge to the prepared PR schema")
		_ = json.NewEncoder(os.Stdout).Encode(report)
		return 4
	}
	chain, err := history.Load(repository.Root, config.BundleRoot, *targetName)
	if err != nil {
		add("invalid_history", err.Error())
		_ = json.NewEncoder(os.Stdout).Encode(report)
		return 4
	}
	manifest := chain.Entries[len(chain.Entries)-1].Artifact.Manifest
	if manifest.BundleID != *bundleID {
		add("incorrect_history_head", fmt.Sprintf("history head is bundle %q, want PR bundle %q", manifest.BundleID, *bundleID))
		_ = json.NewEncoder(os.Stdout).Encode(report)
		return 4
	}
	adminURL := os.Getenv(target.DevDatabaseEnv)
	if adminURL == "" {
		return writeError("source_error", fmt.Errorf("environment variable %s is required", target.DevDatabaseEnv))
	}
	verification, err := verify.Run(ctx, verify.Input{
		AdminURL: adminURL, Chain: chain, BundleID: *bundleID,
		Ignores: manifest.Planner.IgnoreSelectors,
		Options: graphplan.Options{
			ConcurrentIndexes: manifest.Planner.Options.ConcurrentIndexes,
			IfNotExists:       manifest.Planner.Options.IfNotExists,
			IfExists:          manifest.Planner.Options.IfExists,
			CascadeDrops:      manifest.Planner.Options.CascadeDrops,
			SchemaQualifier:   manifest.Planner.Options.SchemaQualifier,
		},
	})
	if err != nil {
		return writeError("verification_error", err)
	}
	report.Verification = &verification
	if verification.Outcome != "verified" {
		add("clone_not_convergent", "executing the complete bundle chain in disposable PostgreSQL did not converge")
		_ = json.NewEncoder(os.Stdout).Encode(report)
		return 4
	}
	report.Outcome = "passed"
	_ = json.NewEncoder(os.Stdout).Encode(report)
	return 0
}

func branchBundleIDs(status gitbase.Status) ([]string, bool) {
	prefix := strings.TrimSuffix(status.HistoryPath, "/") + "/"
	seen := make(map[string]bool)
	working := false
	for _, change := range status.HistoryChanges {
		if change.Ownership != "branch_draft" || !strings.HasPrefix(change.Path, prefix) {
			continue
		}
		relative := strings.TrimPrefix(change.Path, prefix)
		id := strings.SplitN(relative, "/", 2)[0]
		if id != "" {
			seen[id] = true
		}
		working = working || change.Origin == "working_tree"
	}
	ids := make([]string, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids, working
}

func runPR(arguments []string) int {
	return runPRAt(arguments, ".")
}

func runPRAt(arguments []string, start string) int {
	if len(arguments) == 0 {
		return writeError("invalid_invocation", errors.New("usage: onwardpg pr <status|regenerate>"))
	}
	switch arguments[0] {
	case "status":
		return runPRStatusAt(arguments[1:], start)
	case "regenerate":
		return runPRRegenerateAt(arguments[1:], start)
	default:
		return writeError("invalid_invocation", fmt.Errorf("unknown pr command %q", arguments[0]))
	}
}

func runPRStatusAt(arguments []string, start string) int {
	flags := flag.NewFlagSet("pr status", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	base := flags.String("base", "", "exact PR base ref")
	head := flags.String("head", "HEAD", "feature head ref")
	targetName := flags.String("target", "", "configured database target name")
	bundleID := flags.String("bundle", "", "read-only freshness check for this logical bundle")
	configName := flags.String("config", ".onwardpg.toml", "repository configuration path")
	workingTree := flags.Bool("working-tree", true, "include staged, unstaged, and untracked checkout state when head is HEAD")
	if err := flags.Parse(arguments); err != nil {
		return writeError("invalid_invocation", err)
	}
	if *base == "" || *targetName == "" {
		return writeError("invalid_invocation", errors.New("pr status requires --base and --target"))
	}
	if *workingTree && *head != "HEAD" {
		return writeError("invalid_invocation", errors.New("--working-tree may be used only with --head HEAD"))
	}
	repository, err := gitbase.Open(context.Background(), start)
	if err != nil {
		return writeError("git_status_error", err)
	}
	configPath := *configName
	if configPath == ".onwardpg.toml" {
		configPath = filepath.Join(repository.Root, configPath)
	}
	config, err := workspace.Load(configPath)
	if err != nil {
		return writeError("invalid_config", err)
	}
	target, err := config.Target(*targetName)
	if err != nil {
		return writeError("invalid_config", err)
	}
	status, err := repository.Inspect(context.Background(), gitbase.Options{
		BaseRef: *base, HeadRef: *head, HistoryPath: filepath.ToSlash(filepath.Join(config.BundleRoot, *targetName)),
		IncludeWorkingTree: *workingTree, ExcludePaths: []string{config.BundleRoot},
	})
	if err != nil {
		return writeError("git_status_error", err)
	}
	if status.Outcome == "ready" {
		prepared, prepareErr := repository.PreparePRTree(context.Background(), status)
		if prepareErr != nil {
			var conflict *gitbase.MergeConflictError
			if errors.As(prepareErr, &conflict) {
				status.Outcome = "conflicting"
				status.Problems = append(status.Problems, gitbase.Problem{Code: "merge_conflict", Message: conflict.Error()})
			} else {
				return writeError("git_status_error", prepareErr)
			}
		} else {
			_ = prepared.Close()
		}
	}
	if *bundleID != "" && status.Outcome == "ready" {
		return runPRBundleStatus(repository, config, *targetName, *bundleID, target, status)
	}
	_ = json.NewEncoder(os.Stdout).Encode(status)
	if status.Outcome != "ready" {
		return 4
	}
	return 0
}

const prStatusVersion = "onwardpg.pr-status/v1"

type prStatusReport struct {
	ProtocolVersion string            `json:"protocol_version"`
	Outcome         string            `json:"outcome"`
	Repository      gitbase.Status    `json:"repository"`
	Analysis        *prflow.Analysis  `json:"analysis,omitempty"`
	Freshness       *freshness.Report `json:"freshness,omitempty"`
}

func runPRBundleStatus(repository gitbase.Repository, config workspace.Config, targetName, bundleID string, target workspace.Target, status gitbase.Status) int {
	report, err := assessPRBundle(context.Background(), repository, config, targetName, bundleID, target, status)
	if err != nil {
		return writeError("pr_analysis_error", err)
	}
	_ = json.NewEncoder(os.Stdout).Encode(report)
	if report.Outcome == "fresh" {
		return 0
	}
	return 4
}

func assessPRBundle(ctx context.Context, repository gitbase.Repository, config workspace.Config, targetName, bundleID string, target workspace.Target, status gitbase.Status) (prStatusReport, error) {
	destination, err := config.BundlePath(repository.Root, targetName, bundleID)
	if err != nil {
		return prStatusReport{}, err
	}
	artifact, err := bundle.Read(destination)
	if err != nil {
		fresh := freshness.ArtifactStale(err)
		report := prStatusReport{ProtocolVersion: prStatusVersion, Outcome: fresh.Outcome, Repository: status, Freshness: &fresh}
		return report, nil
	}
	if artifact.Manifest.Target != targetName || artifact.Manifest.BundleID != bundleID || artifact.Manifest.Mode != "pr" {
		return prStatusReport{}, errors.New("bundle identity or mode does not match the requested PR target")
	}
	devURL := os.Getenv(target.DevDatabaseEnv)
	if devURL == "" {
		return prStatusReport{}, fmt.Errorf("environment variable %s is required", target.DevDatabaseEnv)
	}
	baseTree, err := repository.PrepareCommitTree(ctx, status.BaseCommit)
	if err != nil {
		return prStatusReport{}, err
	}
	defer baseTree.Close()
	headTree, err := repository.PreparePRTree(ctx, status)
	if err != nil {
		return prStatusReport{}, err
	}
	defer headTree.Close()
	options := graphplan.Options{
		ConcurrentIndexes: artifact.Manifest.Planner.Options.ConcurrentIndexes,
		IfNotExists:       artifact.Manifest.Planner.Options.IfNotExists,
		IfExists:          artifact.Manifest.Planner.Options.IfExists,
		CascadeDrops:      artifact.Manifest.Planner.Options.CascadeDrops,
		SchemaQualifier:   artifact.Manifest.Planner.Options.SchemaQualifier,
	}
	input := prflow.Input{
		BaseRoot: baseTree.Root, HeadRoot: headTree.Root,
		BaseRevision: status.BaseCommit, HeadRevision: status.HeadRevision,
		TargetName: targetName, Target: target, DevDatabaseURL: devURL,
		BundleRoot:     config.BundleRoot,
		PlannerOptions: options, Ignores: artifact.Manifest.Planner.IgnoreSelectors,
	}
	var analysis prflow.Analysis
	if data, exists := artifact.Files["answers.json"]; exists {
		var previous protocol.Answers
		if err := json.Unmarshal(data, &previous); err != nil {
			return prStatusReport{}, fmt.Errorf("decode bundled answers: %w", err)
		}
		questions, questionErr := bundle.DecisionQuestions(artifact)
		if questionErr != nil {
			return prStatusReport{}, questionErr
		}
		analysis, err = prflow.AnalyzeWithReboundAnswers(ctx, input, previous, questions)
	} else {
		analysis, err = prflow.Analyze(ctx, input)
	}
	if err != nil {
		return prStatusReport{}, err
	}
	if analysis.Outcome != "ready" || analysis.Plan == nil {
		report := prStatusReport{ProtocolVersion: prStatusVersion, Outcome: "blocked", Repository: status, Analysis: &analysis}
		return report, nil
	}
	resultDigest, err := bundle.ResultDigest(*analysis.Plan)
	if err != nil {
		return prStatusReport{}, err
	}
	currentPlanner := bundle.PlannerReceipt{
		Version: buildVersion, Options: artifact.Manifest.Planner.Options,
		IgnoreSelectors: append([]string(nil), artifact.Manifest.Planner.IgnoreSelectors...),
	}
	fresh, err := freshness.Assess(artifact, freshness.Observation{
		BaselineRef: status.BaseRef, BaselineRevision: status.BaseCommit, DesiredRevision: status.HeadRevision,
		BaselineFingerprint: analysis.SchemaSquare.BaseCodeFingerprint,
		DesiredFingerprint:  analysis.SchemaSquare.HeadCodeFingerprint,
		HistoryParentDigest: analysis.HistoryDigest,
		Planner:             currentPlanner, ResultDigest: resultDigest,
	})
	if err != nil {
		return prStatusReport{}, err
	}
	report := prStatusReport{ProtocolVersion: prStatusVersion, Outcome: fresh.Outcome, Repository: status, Analysis: &analysis, Freshness: &fresh}
	return report, nil
}

func runPRRegenerateAt(arguments []string, start string) int {
	flags := flag.NewFlagSet("pr regenerate", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	base := flags.String("base", "", "exact PR base ref")
	head := flags.String("head", "HEAD", "feature head ref")
	targetName := flags.String("target", "", "configured database target name")
	bundleID := flags.String("bundle", "", "stable logical bundle identifier")
	configName := flags.String("config", ".onwardpg.toml", "repository configuration path")
	answerFile := flags.String("answers", "", "fingerprint-bound planner answers")
	intentFile := flags.String("intent", "", "Markdown developer intent receipt")
	purpose := flags.String("purpose", "feature", "feature, repair, or contract")
	workingTree := flags.Bool("working-tree", true, "include staged, unstaged, and untracked checkout state when head is HEAD")
	replaceDraft := flags.Bool("replace-draft", false, "replace only a validated unexecuted bundle draft")
	concurrentIndexes := flags.Bool("concurrent-indexes", false, "create standalone indexes concurrently")
	ifNotExists := flags.Bool("if-not-exists", false, "emit IF NOT EXISTS for schema and table creation")
	ifExists := flags.Bool("if-exists", false, "emit IF EXISTS for schema and table drops")
	cascadeDrops := flags.Bool("cascade-drops", false, "emit CASCADE for schema and table drops")
	var ignores stringsFlag
	flags.Var(&ignores, "ignore", "validated catalog selector to exclude")
	if err := flags.Parse(arguments); err != nil {
		return writeError("invalid_invocation", err)
	}
	if *base == "" || *targetName == "" || *bundleID == "" {
		return writeError("invalid_invocation", errors.New("pr regenerate requires --base, --target, and --bundle"))
	}
	if *workingTree && *head != "HEAD" {
		return writeError("invalid_invocation", errors.New("--working-tree may be used only with --head HEAD"))
	}
	repository, err := gitbase.Open(context.Background(), start)
	if err != nil {
		return writeError("git_status_error", err)
	}
	configPath := *configName
	if configPath == ".onwardpg.toml" {
		configPath = filepath.Join(repository.Root, configPath)
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
	destination, err := config.BundlePath(repository.Root, *targetName, *bundleID)
	if err != nil {
		return writeError("invalid_bundle", err)
	}
	answers, err := readAnswers(*answerFile)
	if err != nil {
		return writeError("invalid_answers", err)
	}
	options := graphplan.Options{
		ConcurrentIndexes: *concurrentIndexes, IfNotExists: *ifNotExists,
		IfExists: *ifExists, CascadeDrops: *cascadeDrops,
	}
	excludedPaths := []string{config.BundleRoot}
	excludedPaths = append(excludedPaths, repositoryReceiptPaths(repository.Root, *answerFile, *intentFile)...)
	status, err := repository.Inspect(context.Background(), gitbase.Options{
		BaseRef: *base, HeadRef: *head, HistoryPath: filepath.ToSlash(filepath.Join(config.BundleRoot, *targetName)),
		IncludeWorkingTree: *workingTree, ExcludePaths: excludedPaths,
	})
	if err != nil {
		return writeError("git_status_error", err)
	}
	if status.Outcome != "ready" {
		_ = json.NewEncoder(os.Stdout).Encode(status)
		return 4
	}
	baseTree, err := repository.PrepareCommitTree(context.Background(), status.BaseCommit)
	if err != nil {
		return writeError("git_status_error", err)
	}
	defer baseTree.Close()
	headTree, err := repository.PreparePRTree(context.Background(), status)
	if err != nil {
		return writeError("git_status_error", err)
	}
	defer headTree.Close()
	input := prflow.Input{
		BaseRoot: baseTree.Root, HeadRoot: headTree.Root,
		BaseRevision: status.BaseCommit, HeadRevision: status.HeadRevision,
		TargetName: *targetName, Target: target, DevDatabaseURL: devURL,
		BundleRoot: config.BundleRoot,
		Ignores:    ignores, Answers: answers, PlannerOptions: options,
	}
	var analysis prflow.Analysis
	if *answerFile == "" {
		previous, readErr := bundle.Read(destination)
		switch {
		case readErr == nil && previous.Manifest.BundleID == *bundleID && previous.Manifest.Target == *targetName && previous.Manifest.Mode == "pr":
			if data, exists := previous.Files["answers.json"]; exists {
				var previousAnswers protocol.Answers
				if err := json.Unmarshal(data, &previousAnswers); err != nil {
					return writeError("invalid_bundle", fmt.Errorf("decode bundled answers: %w", err))
				}
				questions, questionErr := bundle.DecisionQuestions(previous)
				if questionErr != nil {
					return writeError("invalid_bundle", questionErr)
				}
				analysis, err = prflow.AnalyzeWithReboundAnswers(context.Background(), input, previousAnswers, questions)
			} else {
				analysis, err = prflow.Analyze(context.Background(), input)
			}
		case readErr == nil:
			return writeError("invalid_bundle", errors.New("existing bundle identity does not match the requested target"))
		case os.IsNotExist(readErr):
			analysis, err = prflow.Analyze(context.Background(), input)
		default:
			return writeError("invalid_bundle", readErr)
		}
	} else {
		analysis, err = prflow.Analyze(context.Background(), input)
	}
	if err != nil {
		return writeError("pr_analysis_error", err)
	}
	if analysis.Outcome == "blocked" {
		_ = json.NewEncoder(os.Stdout).Encode(analysis)
		return 4
	}
	if analysis.Plan == nil {
		return writeError("pr_analysis_error", errors.New("PR analysis did not return a planner result"))
	}
	intent, err := readOptionalFile(*intentFile)
	if err != nil {
		return writeError("invalid_bundle", err)
	}
	var answerReceipt *protocol.Answers
	if *answerFile != "" {
		answerReceipt = &answers
	} else if analysis.Rebind != nil && len(analysis.Rebind.Answers.Answers) > 0 {
		rebound := analysis.Rebind.Answers
		answerReceipt = &rebound
	}
	var questionReceipt []protocol.Question
	if analysis.Rebind != nil {
		questionReceipt = append(questionReceipt, analysis.Rebind.Questions...)
	} else if previous, readErr := bundle.Read(destination); readErr == nil {
		questionReceipt, err = bundle.DecisionQuestions(previous)
		if err != nil {
			return writeError("invalid_bundle", err)
		}
	} else if !os.IsNotExist(readErr) {
		return writeError("invalid_bundle", readErr)
	}
	questionReceipt = mergeQuestions(questionReceipt, analysis.Plan.Questions)
	metadata := bundle.Metadata{
		BundleID: *bundleID, Target: *targetName, Purpose: *purpose, Mode: "pr",
		BaseRef: *base, BaseCommit: status.BaseCommit, HeadRevision: status.HeadRevision,
		BaselineSource: bundle.SourceReceipt{
			Kind: "ddl_export", Description: "base declarative " + analysis.BaseProvenance,
			Fingerprint: analysis.SchemaSquare.BaseCodeFingerprint, GitCommit: status.BaseCommit, PostgresMajor: target.PostgresMajor,
		},
		DesiredSource: bundle.SourceReceipt{
			Kind: "ddl_export", Description: "synthetic head declarative " + analysis.HeadProvenance,
			Fingerprint: analysis.SchemaSquare.HeadCodeFingerprint, GitCommit: status.HeadCommit, PostgresMajor: target.PostgresMajor,
		},
		Planner: bundle.PlannerReceipt{Version: buildVersion, IgnoreSelectors: sortedUniqueStrings(ignores), Options: bundle.PlannerOptions{
			ConcurrentIndexes: options.ConcurrentIndexes, IfNotExists: options.IfNotExists,
			IfExists: options.IfExists, CascadeDrops: options.CascadeDrops,
		}},
		SchemaSquare: &bundle.SchemaSquareReceipt{
			BaseCodeFingerprint:    analysis.SchemaSquare.BaseCodeFingerprint,
			BaseHistoryFingerprint: analysis.SchemaSquare.BaseHistoryFingerprint,
			HeadCodeFingerprint:    analysis.SchemaSquare.HeadCodeFingerprint,
			HeadHistoryFingerprint: analysis.SchemaSquare.HeadHistoryFingerprint,
			BaseHistoryDigest:      analysis.HistoryDigest,
			BaseIntegrity:          analysis.SchemaSquare.BaseIntegrity,
			HeadHistoryFidelity:    analysis.SchemaSquare.HeadHistoryFidelity,
		},
		HistoryParentDigest: analysis.HistoryDigest,
	}
	generation, attempt, err := bundle.NextCoordinates(destination, metadata, *analysis.Plan)
	if err != nil {
		return writeError("invalid_bundle", err)
	}
	metadata.Generation = generation
	artifact, err := bundle.Build(bundle.Input{
		Metadata: metadata,
		Result:   *analysis.Plan, Answers: answerReceipt, Questions: questionReceipt, Intent: intent, Attempt: attempt,
	})
	if err != nil {
		return writeError("invalid_bundle", err)
	}
	if err := bundle.Write(destination, artifact, bundle.WriteOptions{ReplaceDraft: *replaceDraft}); err != nil {
		return writeError("bundle_write_failed", err)
	}
	relative, err := filepath.Rel(repository.Root, destination)
	if err != nil {
		return writeError("bundle_write_failed", err)
	}
	analysis.Bundle = &prflow.BundleOutput{
		Path: filepath.ToSlash(relative), BundleID: *bundleID, Generation: generation, State: string(analysis.Plan.Status),
	}
	_ = json.NewEncoder(os.Stdout).Encode(analysis)
	switch analysis.Plan.Status {
	case protocol.Planned:
		return 0
	case protocol.NeedsInput:
		return 2
	case protocol.Unsupported:
		return 3
	default:
		return 1
	}
}

func runPlan(arguments []string) int {
	flags := flag.NewFlagSet("plan", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	from, to, devURL, answerFile := flags.String("from", "", ""), flags.String("to", "", ""), flags.String("dev-url", "", ""), flags.String("answers", "", "")
	concurrentIndexes := flags.Bool("concurrent-indexes", false, "create standalone indexes concurrently")
	ifNotExists := flags.Bool("if-not-exists", false, "emit IF NOT EXISTS for schema and table creation")
	ifExists := flags.Bool("if-exists", false, "emit IF EXISTS for schema and table drops")
	cascadeDrops := flags.Bool("cascade-drops", false, "emit CASCADE for schema and table drops")
	sqlOutput := flags.Bool("sql", false, "print planned SQL instead of JSON")
	indent := flags.String("indent", "", "prefix each rendered SQL line")
	unsortedDump := flags.Bool("unsorted-dump", false, "preserve dump order instead of dependency sorting")
	bundleDir := flags.String("bundle", "", "write a versioned migration receipt bundle to this directory")
	bundleID := flags.String("bundle-id", "", "stable logical bundle identifier")
	target := flags.String("target", "", "configured database target name")
	bundlePurpose := flags.String("bundle-purpose", "feature", "feature, repair, or contract")
	bundleMode := flags.String("bundle-mode", "pr", "develop, pr, release, or verify")
	baseRef := flags.String("base-ref", "", "exact logical PR base ref recorded in the bundle")
	baseCommit := flags.String("base-commit", "", "full PR base commit SHA recorded in the bundle")
	headRevision := flags.String("head-revision", "", "full head commit SHA or dirty-tree digest")
	intentFile := flags.String("intent", "", "Markdown intent receipt to include in the bundle")
	replaceDraft := flags.Bool("replace-draft", false, "replace only a validated unexecuted draft bundle")
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
	if *bundleDir == "" && (*bundleID != "" || *target != "" || *bundlePurpose != "feature" || *bundleMode != "pr" || *baseRef != "" || *baseCommit != "" || *headRevision != "" || *intentFile != "" || *replaceDraft) {
		return writeError("invalid_invocation", errors.New("bundle receipt flags require --bundle"))
	}
	if *unsortedDump {
		return writeError("invalid_invocation", errors.New("--unsorted-dump requires a complete typed object order and is unavailable for CLI URL/DDL sources"))
	}
	answers, err := readAnswers(*answerFile)
	if err != nil {
		return writeError("invalid_answers", err)
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
	result, err := graphplan.Build(current, desired, answers, options)
	if err != nil {
		return writeError("planning_error", err)
	}
	if *bundleDir != "" {
		if *bundleID == "" || *target == "" {
			return writeError("invalid_bundle", errors.New("--bundle requires --bundle-id and --target"))
		}
		intent, err := readOptionalFile(*intentFile)
		if err != nil {
			return writeError("invalid_bundle", err)
		}
		var answerReceipt *protocol.Answers
		if *answerFile != "" {
			answerReceipt = &answers
		}
		metadata := bundle.Metadata{
			BundleID: *bundleID, Target: *target,
			Purpose: *bundlePurpose, Mode: *bundleMode, BaseRef: *baseRef,
			BaseCommit: *baseCommit, HeadRevision: *headRevision,
			BaselineSource: sourceReceipt(fromSpec, result.CurrentFingerprint, "current"),
			DesiredSource:  sourceReceipt(toSpec, result.DesiredFingerprint, "desired"),
			Planner: bundle.PlannerReceipt{Version: buildVersion, IgnoreSelectors: sortedUniqueStrings(ignores), Options: bundle.PlannerOptions{
				ConcurrentIndexes: options.ConcurrentIndexes, IfNotExists: options.IfNotExists,
				IfExists: options.IfExists, CascadeDrops: options.CascadeDrops,
				SchemaQualifier: options.SchemaQualifier,
			}},
		}
		generation, attempt, err := bundle.NextCoordinates(*bundleDir, metadata, result)
		if err != nil {
			return writeError("invalid_bundle", err)
		}
		metadata.Generation = generation
		artifact, err := bundle.Build(bundle.Input{
			Metadata: metadata,
			Result:   result, Answers: answerReceipt, Questions: result.Questions, Intent: intent, Attempt: attempt,
		})
		if err != nil {
			return writeError("invalid_bundle", err)
		}
		if err := bundle.Write(*bundleDir, artifact, bundle.WriteOptions{ReplaceDraft: *replaceDraft}); err != nil {
			return writeError("bundle_write_failed", err)
		}
	}
	if *sqlOutput && result.Status == protocol.Planned {
		_, _ = fmt.Fprintln(os.Stdout, protocol.RenderSQL(result, *indent))
	} else {
		_ = json.NewEncoder(os.Stdout).Encode(result)
	}
	return resultExitCode(result.Status)
}

func resultExitCode(status protocol.Status) int {
	switch status {
	case protocol.Planned:
		return 0
	case protocol.NeedsInput:
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
	config, err := workspace.Load(*name)
	if err != nil {
		return writeError("invalid_config", err)
	}
	targets := make([]string, 0, len(config.Targets))
	for target := range config.Targets {
		targets = append(targets, target)
	}
	sort.Strings(targets)
	_ = json.NewEncoder(os.Stdout).Encode(struct {
		ProtocolVersion string   `json:"protocol_version"`
		Status          string   `json:"status"`
		ConfigVersion   int      `json:"config_version"`
		Targets         []string `json:"targets"`
	}{ProtocolVersion: "onwardpg.config-check/v1", Status: "valid", ConfigVersion: config.Version, Targets: targets})
	return 0
}

func readAnswers(path string) (protocol.Answers, error) {
	if path == "" {
		return protocol.Answers{}, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return protocol.Answers{}, err
	}
	var answers protocol.Answers
	if err := json.Unmarshal(data, &answers); err != nil {
		return protocol.Answers{}, err
	}
	return answers, nil
}

func readOptionalFile(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read intent file: %w", err)
	}
	return string(data), nil
}

func sourceReceipt(spec source.Spec, fingerprint, role string) bundle.SourceReceipt {
	description, kind := role+" database", "database"
	if spec.Kind == "ddl" {
		kind = "ddl"
		description = role + " DDL " + filepath.Base(spec.Value)
	}
	return bundle.SourceReceipt{Kind: kind, Description: description, Fingerprint: fingerprint}
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

func mergeQuestions(first, second []protocol.Question) []protocol.Question {
	result := append([]protocol.Question(nil), first...)
	seen := make(map[string]bool, len(result))
	for _, question := range result {
		seen[question.Kind+":"+question.Key] = true
	}
	for _, question := range second {
		id := question.Kind + ":" + question.Key
		if !seen[id] {
			result = append(result, question)
			seen[id] = true
		}
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].Kind+":"+result[i].Key < result[j].Kind+":"+result[j].Key
	})
	return result
}

func repositoryReceiptPaths(root string, names ...string) []string {
	var paths []string
	for _, name := range names {
		if name == "" {
			continue
		}
		absolute, err := filepath.Abs(name)
		if err != nil {
			continue
		}
		relative, err := filepath.Rel(root, absolute)
		if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			continue
		}
		paths = append(paths, filepath.ToSlash(relative))
	}
	return sortedUniqueStrings(paths)
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
