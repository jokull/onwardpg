package workspace

import (
	"context"
	"strings"
	"testing"
)

func TestObserverPolicyRequiresSuccessfulConfiguredDatabaseInspection(t *testing.T) {
	const env = "ONWARDPG_OBSERVER_POLICY_TEST_URL"
	target := Target{DevDatabaseEnv: env, Ignore: []string{"table:public.django_migrations"}}
	for _, test := range []struct{ name, url, message string }{
		{"missing environment", "", "is required"},
		{"unavailable database", "postgres://postgres@127.0.0.1:1/postgres?sslmode=disable&connect_timeout=1", "inspect development observer policy"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv(env, test.url)
			selectors, err := ObserverIgnoreSelectors(context.Background(), target)
			if err == nil || !strings.Contains(err.Error(), test.message) || len(selectors) != 0 {
				t.Fatalf("unvalidated policy = %#v, %v", selectors, err)
			}
		})
	}
	selectors, err := ObserverIgnoreSelectors(context.Background(), Target{Ignore: target.Ignore})
	if err != nil || len(selectors) != 0 {
		t.Fatalf("scratch-only target gained live policy: %#v, %v", selectors, err)
	}
}
