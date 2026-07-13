package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/jokull/onwardpg/internal/graphplan"
	"github.com/jokull/onwardpg/internal/protocol"
	"github.com/jokull/onwardpg/internal/source"
)

func main() { os.Exit(run()) }

func run() int {
	if len(os.Args) < 2 || os.Args[1] != "plan" {
		fmt.Fprintln(os.Stderr, "usage: onwardpg plan --from SOURCE --to SOURCE [--dev-url URL] [--answers FILE] [--ignore SELECTOR] [--schema-qualifier SCHEMA]")
		return 1
	}
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
	var schemaQualifier optionalString
	flags.Var(&schemaQualifier, "schema-qualifier", "scope to one schema and render names using this qualifier (empty means unqualified)")
	var ignores stringsFlag
	flags.Var(&ignores, "ignore", "selector to exclude")
	if err := flags.Parse(os.Args[2:]); err != nil || *from == "" || *to == "" {
		return 1
	}
	if *unsortedDump {
		return writeError(errors.New("--unsorted-dump requires an adapter-supplied object order and is unavailable for CLI URL/DDL sources"))
	}
	answers, err := readAnswers(*answerFile)
	if err != nil {
		return writeError(err)
	}
	ctx := context.Background()
	current, err := source.LoadGraphForComparison(ctx, source.Parse(*from), *devURL, ignores)
	if err != nil {
		return writeError(err)
	}
	desired, err := source.LoadGraphForComparison(ctx, source.Parse(*to), *devURL, ignores)
	if err != nil {
		return writeError(err)
	}
	if err := source.ValidateIgnoreSelectors(ignores, current, desired); err != nil {
		return writeError(err)
	}
	options := graphplan.Options{ConcurrentIndexes: *concurrentIndexes, IfNotExists: *ifNotExists, IfExists: *ifExists, CascadeDrops: *cascadeDrops, UnsortedDump: *unsortedDump}
	if schemaQualifier.set {
		options.SchemaQualifier = &schemaQualifier.value
	}
	result, err := graphplan.Build(current, desired, answers, options)
	if err != nil {
		return writeError(err)
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

func writeError(err error) int {
	_ = json.NewEncoder(os.Stdout).Encode(map[string]string{"status": "error", "error": err.Error()})
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
