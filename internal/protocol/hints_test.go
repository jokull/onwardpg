package protocol

import (
	"strings"
	"testing"
)

func TestDecodeHintAcceptsMinimalRename(t *testing.T) {
	hint, err := DecodeHint([]byte(`{"kind":"rename","object":"column","from":["public","users","name"],"to":["public","users","display_name"]}`))
	if err != nil {
		t.Fatal(err)
	}
	if hint.Kind != "rename" || hint.Object != "column" || hint.From[2] != "name" || hint.To[2] != "display_name" {
		t.Fatalf("hint = %#v", hint)
	}
}

func TestDecodeHintAcceptsTableIdentityOnly(t *testing.T) {
	hint, err := DecodeHint([]byte(`{"kind":"identity","object":"table","from":["app","accounts"],"to":["app","customers"]}`))
	if err != nil || hint.Kind != "identity" {
		t.Fatalf("table identity = %#v, %v", hint, err)
	}
	if _, err := DecodeHint([]byte(`{"kind":"identity","object":"column","from":["app","accounts","old"],"to":["app","accounts","new"]}`)); err == nil {
		t.Fatal("column identity must be rejected until its candidacy slice exists")
	}
}

func TestDecodeHintInfersColumnAndNotNullFromKinds(t *testing.T) {
	typeChange, err := DecodeHint([]byte(`{"kind":"type_change","name":["public","events","occurred_on"],"strategy":"manual_sql"}`))
	if err != nil {
		t.Fatal(err)
	}
	rollout, err := DecodeHint([]byte(`{"kind":"rollout","name":["public","events","occurred_on"],"strategy":"staged_with_backfill"}`))
	if err != nil {
		t.Fatal(err)
	}
	if typeChange.Object != "" || rollout.Object != "" || typeChange.Name[2] != "occurred_on" || rollout.Strategy != "staged_with_backfill" {
		t.Fatalf("type_change=%#v rollout=%#v", typeChange, rollout)
	}
	typeSplit, err := DecodeHint([]byte(`{"kind":"type_change","name":["public","events","occurred_on"],"strategy":"split_plan"}`))
	if err != nil || typeSplit.Strategy != "split_plan" {
		t.Fatalf("type split=%#v err=%v", typeSplit, err)
	}
	renameBackfill, err := DecodeHint([]byte(`{"kind":"rename_backfill","name":["public","events","old_name"],"strategy":"single_transaction"}`))
	if err != nil || renameBackfill.Strategy != "single_transaction" {
		t.Fatalf("rename backfill=%#v err=%v", renameBackfill, err)
	}
}

func TestDecodeHintRejectsInferableAndUnknownFields(t *testing.T) {
	for _, document := range []string{
		`{"kind":"rename","object":"column","from":["public","users","name"],"to":["public","users","display_name"],"current_fingerprint":"sha256:no"}`,
		`{"kind":"drop","object":"column","object":"table","name":["public","users","name"]}`,
		`{"kind":"drop","object":"column","name":["public","users","name"],"strategy":"direct"}`,
		`{"kind":"rename","object":"column","from":["public","users"],"to":["public","users","display_name"]}`,
		`{"kind":"rename","object":"column","from":["public","users","name"],"to":["public","users","name"]}`,
		`{"kind":"type_change","object":"column","name":["public","events","occurred_on"],"strategy":"manual_sql"}`,
		`{"kind":"type_change","name":["public","events","occurred_on"],"strategy":"direct"}`,
		`{"kind":"rollout","object":"column","name":["public","events","occurred_on"],"strategy":"staged"}`,
		`{"kind":"rollout","name":["public","events","occurred_on"],"change":"not_null","strategy":"staged"}`,
	} {
		if _, err := DecodeHint([]byte(document)); err == nil {
			t.Fatalf("expected %s to fail", document)
		}
	}
}

func TestDecodeHintsRejectsDuplicateSemanticIntent(t *testing.T) {
	document := `[
		{"kind":"drop","object":"column","name":["public","users","legacy"]},
		{"kind":"drop","object":"column","name":["public","users","legacy"]}
	]`
	_, err := DecodeHints([]byte(document))
	if err == nil || !strings.Contains(err.Error(), "duplicates") {
		t.Fatalf("error = %v", err)
	}
}

func TestCanonicalHintsSortsWithoutChangingMeaning(t *testing.T) {
	hints, err := CanonicalHints([]Hint{
		{Kind: "drop", Object: "column", Name: []string{"public", "users", "z"}},
		{Kind: "drop", Object: "column", Name: []string{"public", "users", "a"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if hints[0].Name[2] != "a" || hints[1].Name[2] != "z" {
		t.Fatalf("hints = %#v", hints)
	}
}
