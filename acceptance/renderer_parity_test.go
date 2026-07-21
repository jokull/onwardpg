package acceptance_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jokull/onwardpg/internal/testkit"
)

func TestRendererParityRequiredColumnDecision(t *testing.T) {
	requireAcceptance(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	adminURL := os.Getenv(acceptanceDatabaseEnv)
	workspace, err := testkit.NewWorkspace(t.TempDir(), "app", acceptanceDatabaseEnv)
	if err != nil {
		t.Fatal(err)
	}
	trackAcceptanceWorkspace(t, workspace)
	if err := workspace.WriteSchema([]byte("CREATE SCHEMA app; CREATE TABLE app.bookings (id bigint PRIMARY KEY);\n")); err != nil {
		t.Fatal(err)
	}
	environment := map[string]string{acceptanceDatabaseEnv: adminURL}
	runOK(t, ctx, workspace.Root, environment, "init", "--target", "app", "--bundle", "baseline")
	if err := workspace.WriteSchema([]byte("CREATE SCHEMA app; CREATE TABLE app.bookings (id bigint PRIMARY KEY, status text NOT NULL);\n")); err != nil {
		t.Fatal(err)
	}
	jsonResult := runExit(t, ctx, workspace.Root, environment, 2, "plan", "renderer-status", "--target", "app", "--output", "json")
	var envelope testkit.PlanEnvelope
	if err := jsonResult.DecodeJSON(&envelope); err != nil {
		t.Fatal(err)
	}
	textResult := runExit(t, ctx, workspace.Root, environment, 2, "plan", "--target", "app", "--output", "text")
	assertTextCoversEnvelope(t, textResult.Stdout, envelope)
	if strings.Contains(jsonResult.Stdout, "protocol_version") || strings.Contains(textResult.Stdout, "protocol_version") {
		t.Fatal("unused protocol-version scaffolding reappeared in plan output")
	}

	sqlResult := runExit(t, ctx, workspace.Root, environment, 2, "plan", "--target", "app", "--output", "sql")
	for _, line := range strings.Split(strings.TrimSpace(sqlResult.Stdout), "\n") {
		if line != "" && !strings.HasPrefix(line, "--") {
			t.Fatalf("SQL mode mixed a non-SQL instruction into stdout:\n%s", sqlResult.Stdout)
		}
	}
	if strings.Contains(sqlResult.Stdout, "protocol_version") || strings.Contains(sqlResult.Stdout, "{") || strings.TrimSpace(sqlResult.Stderr) != "" {
		t.Fatalf("SQL mode mixed structured or diagnostic output into its executable-only stream: stdout=%q stderr=%q", sqlResult.Stdout, sqlResult.Stderr)
	}
}

func assertTextCoversEnvelope(t *testing.T, text string, envelope testkit.PlanEnvelope) {
	t.Helper()
	for _, token := range []string{
		"status: " + envelope.Status,
		"durable: " + envelope.Durable.Status,
		"development: " + envelope.Development.Status,
	} {
		if !strings.Contains(text, token) {
			t.Errorf("text renderer lacks %q:\n%s", token, text)
		}
	}
	for _, finding := range envelope.Durable.Findings {
		if !strings.Contains(text, "finding "+finding.Code+":") || !strings.Contains(text, finding.Message) {
			t.Errorf("text renderer lacks finding %#v:\n%s", finding, text)
		}
	}
	for _, action := range envelope.NextActions {
		switch action.Kind {
		case "semantic_hint", "deferred_hint":
			for _, choice := range action.Choices {
				for _, argument := range choice.Argv {
					if !strings.Contains(text, argument) {
						t.Errorf("text renderer lacks %s argv argument %q:\n%s", action.Kind, argument, text)
					}
				}
			}
		case "edit_file", "merge_edit_conflict":
			if action.Path != "" && !strings.Contains(text, action.Path) {
				t.Errorf("text renderer lacks action path %q:\n%s", action.Path, text)
			}
		}
	}
}
