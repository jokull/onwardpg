package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
)

type testEvent struct {
	Action string `json:"Action"`
	Test   string `json:"Test"`
}

func main() {
	logPath := flag.String("log", "", "go test -json event log")
	requiredValue := flag.String("require", "", "comma-separated authoritative top-level tests")
	flag.Parse()
	if *logPath == "" || *requiredValue == "" {
		fatal("-log and -require are required")
	}
	var requiredNames []string
	for _, name := range strings.Split(*requiredValue, ",") {
		name = strings.TrimSpace(name)
		if name == "" {
			fatal("required test names must not be empty")
		}
		requiredNames = append(requiredNames, name)
	}
	file, err := os.Open(*logPath)
	if err != nil {
		fatal(err.Error())
	}
	defer file.Close()
	if err := checkEvents(file, requiredNames); err != nil {
		fatal(err.Error())
	}
	fmt.Printf("acceptance event receipt: %d authoritative tests passed without skips\n", len(requiredNames))
}

func checkEvents(reader io.Reader, requiredNames []string) error {
	required := make(map[string]bool, len(requiredNames))
	for _, name := range requiredNames {
		if name == "" {
			return fmt.Errorf("required test names must not be empty")
		}
		required[name] = false
	}
	scanner := bufio.NewScanner(reader)
	for scanner.Scan() {
		var event testEvent
		if json.Unmarshal(scanner.Bytes(), &event) != nil || event.Test == "" {
			continue
		}
		if event.Action == "skip" {
			return fmt.Errorf("selected acceptance test skipped: %s", event.Test)
		}
		if _, tracked := required[event.Test]; !tracked {
			continue
		}
		switch event.Action {
		case "pass":
			required[event.Test] = true
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	var missing []string
	for name, passed := range required {
		if !passed {
			missing = append(missing, name)
		}
	}
	if len(missing) != 0 {
		sort.Strings(missing)
		return fmt.Errorf("authoritative acceptance tests did not pass: %s", strings.Join(missing, ", "))
	}
	return nil
}

func fatal(message string) {
	fmt.Fprintln(os.Stderr, message)
	os.Exit(1)
}
