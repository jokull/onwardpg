package main

import (
	"fmt"
	"strings"
	"testing"
)

func TestRenderFormulaUsesEverySupportedHomebrewArchive(t *testing.T) {
	version := "0.1.0-preview.1"
	var checksums strings.Builder
	for index, target := range [][2]string{{"darwin", "amd64"}, {"darwin", "arm64"}, {"linux", "amd64"}, {"linux", "arm64"}} {
		fmt.Fprintf(&checksums, "%s  ./%s\n", strings.Repeat(fmt.Sprintf("%x", index+1), 64), artifactName(version, target[0], target[1]))
	}
	formula, err := renderFormula("v"+version, strings.NewReader(checksums.String()))
	if err != nil {
		t.Fatal(err)
	}
	text := string(formula)
	for _, fragment := range []string{
		"class Onwardpg < Formula",
		`version "0.1.0-preview.1"`,
		"releases/download/v0.1.0-preview.1/onwardpg_0.1.0-preview.1_darwin_arm64.tar.gz",
		"releases/download/v0.1.0-preview.1/onwardpg_0.1.0-preview.1_linux_amd64.tar.gz",
		"if Hardware::CPU.intel? && Hardware::CPU.is_64_bit?",
		`bin.install "onwardpg"`,
		`assert_match '"version":"v0.1.0-preview.1"'`,
	} {
		if !strings.Contains(text, fragment) {
			t.Fatalf("formula missing %q:\n%s", fragment, text)
		}
	}
}

func TestRenderFormulaRejectsMissingArchiveChecksum(t *testing.T) {
	_, err := renderFormula("v0.1.0-preview.1", strings.NewReader(strings.Repeat("a", 64)+"  ./onwardpg_0.1.0-preview.1_darwin_amd64.tar.gz\n"))
	if err == nil || !strings.Contains(err.Error(), "darwin_arm64") {
		t.Fatalf("missing checksum error = %v", err)
	}
}
