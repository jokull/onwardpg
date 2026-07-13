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

	"github.com/jokull/onwardpg/internal/bundle"
	"github.com/jokull/onwardpg/internal/freshness"
	"github.com/jokull/onwardpg/internal/gitbase"
	"github.com/jokull/onwardpg/internal/graphplan"
	"github.com/jokull/onwardpg/internal/prflow"
	"github.com/jokull/onwardpg/internal/protocol"
	"github.com/jokull/onwardpg/internal/source"
	"github.com/jokull/onwardpg/internal/workspace"
)

var buildVersion = "dev"

func main() { os.Exit(run()) }

func run() int {
	if len(os.Args) < 2 {
		return writeError("invalid_invocation", errors.New("usage: onwardpg <plan|config|pr>"))
	}
	switch os.Args[1] {
	case "plan":
		return runPlan(os.Args[2:])
	case "config":
		return runConfig(os.Args[2:])
	case "pr":
		return runPR(os.Args[2:])
	default:
		return writeError("invalid_invocation", fmt.Errorf("unknown command %q", os.Args[1]))
	}
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
		BaseRef: *base, HeadRef: *head, MigrationPath: target.MigrationPath,
		IncludeWorkingTree: *workingTree, ExcludePaths: []string{config.BundleRoot, target.MigrationPath},
	})
	if err != nil {
		return writeError("git_status_error", err)
	}
	if *bundleID != "" && status.Outcome == "ready" {
		return runPRBundleStatus(repository, config, *targetName, *bundleID, target, status)
	}
	_ = json.NewEncoder(os.Stdout).Encode(status)
	if status.Outcome == "blocked" {
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
	destination, err := config.BundlePath(repository.Root, targetName, bundleID)
	if err != nil {
		return writeError("invalid_bundle", err)
	}
	artifact, err := bundle.Read(destination)
	if err != nil {
		fresh := freshness.ArtifactStale(err)
		report := prStatusReport{ProtocolVersion: prStatusVersion, Outcome: fresh.Outcome, Repository: status, Freshness: &fresh}
		_ = json.NewEncoder(os.Stdout).Encode(report)
		return 4
	}
	if artifact.Manifest.Target != targetName || artifact.Manifest.BundleID != bundleID || artifact.Manifest.Mode != "pr" {
		return writeError("invalid_bundle", errors.New("bundle identity or mode does not match the requested PR target"))
	}
	devURL := os.Getenv(target.DevDatabaseEnv)
	if devURL == "" {
		return writeError("source_error", fmt.Errorf("environment variable %s is required", target.DevDatabaseEnv))
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
		PlannerOptions: options, Ignores: artifact.Manifest.Planner.IgnoreSelectors,
	}
	analysis, err := prflow.Analyze(context.Background(), input)
	if err != nil {
		return writeError("pr_analysis_error", err)
	}
	if analysis.Outcome == "blocked" || analysis.Plan == nil {
		report := prStatusReport{ProtocolVersion: prStatusVersion, Outcome: "blocked", Repository: status, Analysis: &analysis}
		_ = json.NewEncoder(os.Stdout).Encode(report)
		return 4
	}
	if data, exists := artifact.Files["answers.json"]; exists &&
		analysis.Plan.CurrentFingerprint == artifact.Manifest.BaselineSource.Fingerprint &&
		analysis.Plan.DesiredFingerprint == artifact.Manifest.DesiredSource.Fingerprint {
		var answers protocol.Answers
		if err := json.Unmarshal(data, &answers); err != nil {
			return writeError("invalid_bundle", fmt.Errorf("decode bundled answers: %w", err))
		}
		input.Answers = answers
		analysis, err = prflow.Analyze(context.Background(), input)
		if err != nil {
			return writeError("pr_analysis_error", err)
		}
	}
	resultDigest, err := bundle.ResultDigest(*analysis.Plan)
	if err != nil {
		return writeError("invalid_bundle", err)
	}
	currentPlanner := bundle.PlannerReceipt{
		Version: buildVersion, Options: artifact.Manifest.Planner.Options,
		IgnoreSelectors: append([]string(nil), artifact.Manifest.Planner.IgnoreSelectors...),
	}
	fresh, err := freshness.Assess(artifact, freshness.Observation{
		BaselineRef: status.BaseRef, BaselineRevision: status.BaseCommit, DesiredRevision: status.HeadRevision,
		BaselineFingerprint: analysis.SchemaSquare.BaseCodeFingerprint,
		DesiredFingerprint:  analysis.SchemaSquare.HeadCodeFingerprint,
		Planner:             currentPlanner, ResultDigest: resultDigest,
	})
	if err != nil {
		return writeError("freshness_error", err)
	}
	report := prStatusReport{ProtocolVersion: prStatusVersion, Outcome: fresh.Outcome, Repository: status, Analysis: &analysis, Freshness: &fresh}
	_ = json.NewEncoder(os.Stdout).Encode(report)
	if fresh.Outcome == "fresh" {
		return 0
	}
	return 4
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
	answers, err := readAnswers(*answerFile)
	if err != nil {
		return writeError("invalid_answers", err)
	}
	options := graphplan.Options{
		ConcurrentIndexes: *concurrentIndexes, IfNotExists: *ifNotExists,
		IfExists: *ifExists, CascadeDrops: *cascadeDrops,
	}
	status, err := repository.Inspect(context.Background(), gitbase.Options{
		BaseRef: *base, HeadRef: *head, MigrationPath: target.MigrationPath,
		IncludeWorkingTree: *workingTree, ExcludePaths: []string{config.BundleRoot, target.MigrationPath},
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
	analysis, err := prflow.Analyze(context.Background(), prflow.Input{
		BaseRoot: baseTree.Root, HeadRoot: headTree.Root,
		BaseRevision: status.BaseCommit, HeadRevision: status.HeadRevision,
		TargetName: *targetName, Target: target, DevDatabaseURL: devURL,
		Ignores: ignores, Answers: answers, PlannerOptions: options,
	})
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
	destination, err := config.BundlePath(repository.Root, *targetName, *bundleID)
	if err != nil {
		return writeError("invalid_bundle", err)
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
	}
	generation, attempt, err := bundle.NextCoordinates(destination, metadata, *analysis.Plan)
	if err != nil {
		return writeError("invalid_bundle", err)
	}
	metadata.Generation = generation
	artifact, err := bundle.Build(bundle.Input{
		Metadata: metadata,
		Result:   *analysis.Plan, Answers: answerReceipt, Intent: intent, Attempt: attempt,
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
			Result:   result, Answers: answerReceipt, Intent: intent, Attempt: attempt,
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
	switch result.Status {
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
