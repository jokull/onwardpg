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
		IncludeWorkingTree: *workingTree, ExcludePaths: []string{config.BundleRoot},
	})
	if err != nil {
		return writeError("git_status_error", err)
	}
	_ = json.NewEncoder(os.Stdout).Encode(status)
	if status.Outcome == "blocked" {
		return 4
	}
	return 0
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
	analysis, err := prflow.Analyze(context.Background(), prflow.Input{
		Repository: repository, TargetName: *targetName, Target: target,
		BaseRef: *base, HeadRef: *head, IncludeWorkingTree: *workingTree,
		ExcludePaths: []string{config.BundleRoot}, DevDatabaseURL: devURL,
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
	generation, attempt, err := bundle.NextCoordinates(destination)
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
	artifact, err := bundle.Build(bundle.Input{
		Metadata: bundle.Metadata{
			BundleID: *bundleID, Generation: generation, Target: *targetName, Purpose: *purpose, Mode: "pr",
			BaseRef: *base, BaseCommit: analysis.Git.BaseCommit, HeadRevision: analysis.Git.HeadRevision,
			BaselineSource: bundle.SourceReceipt{
				Kind: "adapter", Description: "base declarative " + analysis.BaseProvenance,
				Fingerprint: analysis.SchemaSquare.BaseCodeFingerprint, GitCommit: analysis.Git.BaseCommit, PostgresMajor: target.PostgresMajor,
			},
			DesiredSource: bundle.SourceReceipt{
				Kind: "adapter", Description: "synthetic head declarative " + analysis.HeadProvenance,
				Fingerprint: analysis.SchemaSquare.HeadCodeFingerprint, GitCommit: analysis.Git.HeadCommit, PostgresMajor: target.PostgresMajor,
			},
			Planner: bundle.PlannerReceipt{Version: buildVersion, Options: bundle.PlannerOptions{
				ConcurrentIndexes: options.ConcurrentIndexes, IfNotExists: options.IfNotExists,
				IfExists: options.IfExists, CascadeDrops: options.CascadeDrops,
			}},
			SchemaSquare: &bundle.SchemaSquareReceipt{
				BaseCodeFingerprint:    analysis.SchemaSquare.BaseCodeFingerprint,
				BaseHistoryFingerprint: analysis.SchemaSquare.BaseHistoryFingerprint,
				HeadCodeFingerprint:    analysis.SchemaSquare.HeadCodeFingerprint,
				BaseIntegrity:          analysis.SchemaSquare.BaseIntegrity,
				HeadArtifactFidelity:   analysis.SchemaSquare.HeadArtifactFidelity,
			},
		},
		Result: *analysis.Plan, Answers: answerReceipt, Intent: intent, Attempt: attempt,
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
	if *unsortedDump {
		return writeError("invalid_invocation", errors.New("--unsorted-dump requires an adapter-supplied object order and is unavailable for CLI URL/DDL sources"))
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
		generation, attempt, err := bundle.NextCoordinates(*bundleDir)
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
		artifact, err := bundle.Build(bundle.Input{
			Metadata: bundle.Metadata{
				BundleID: *bundleID, Generation: generation, Target: *target,
				Purpose: *bundlePurpose, Mode: *bundleMode, BaseRef: *baseRef,
				BaseCommit: *baseCommit, HeadRevision: *headRevision,
				BaselineSource: sourceReceipt(fromSpec, result.CurrentFingerprint, "current"),
				DesiredSource:  sourceReceipt(toSpec, result.DesiredFingerprint, "desired"),
				Planner: bundle.PlannerReceipt{Version: buildVersion, Options: bundle.PlannerOptions{
					ConcurrentIndexes: options.ConcurrentIndexes, IfNotExists: options.IfNotExists,
					IfExists: options.IfExists, CascadeDrops: options.CascadeDrops,
					SchemaQualifier: options.SchemaQualifier,
				}},
			},
			Result: result, Answers: answerReceipt, Intent: intent, Attempt: attempt,
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
		Status        string   `json:"status"`
		ConfigVersion int      `json:"config_version"`
		Targets       []string `json:"targets"`
	}{Status: "valid", ConfigVersion: config.Version, Targets: targets})
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
