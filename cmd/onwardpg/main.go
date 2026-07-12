package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/jokull/onwardpg/internal/plan"
	"github.com/jokull/onwardpg/internal/protocol"
	"github.com/jokull/onwardpg/internal/source"
)

func main() { os.Exit(run()) }

func run() int {
	if len(os.Args) < 2 || os.Args[1] != "plan" {
		fmt.Fprintln(os.Stderr, "usage: onwardpg plan --from SOURCE --to SOURCE [--dev-url URL] [--answers FILE] [--ignore SELECTOR]")
		return 1
	}
	flags := flag.NewFlagSet("plan", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	from, to, devURL, answerFile := flags.String("from", "", ""), flags.String("to", "", ""), flags.String("dev-url", "", ""), flags.String("answers", "", "")
	var ignores stringsFlag
	flags.Var(&ignores, "ignore", "selector to exclude")
	if err := flags.Parse(os.Args[2:]); err != nil || *from == "" || *to == "" {
		return 1
	}
	answers, err := readAnswers(*answerFile)
	if err != nil {
		return writeError(err)
	}
	ctx := context.Background()
	current, err := source.Load(ctx, source.Parse(*from), *devURL, ignores)
	if err != nil {
		return writeError(err)
	}
	desired, err := source.Load(ctx, source.Parse(*to), *devURL, ignores)
	if err != nil {
		return writeError(err)
	}
	result := plan.Build(current, desired, answers)
	_ = json.NewEncoder(os.Stdout).Encode(result)
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
