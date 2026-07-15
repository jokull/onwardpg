package main

import (
	"bufio"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var releaseVersionPattern = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+(?:[.-][0-9A-Za-z.-]+)?$`)

type releaseArtifact struct {
	os       string
	arch     string
	checksum string
}

func main() {
	version := flag.String("version", "", "release tag, for example v0.1.0-preview.1")
	checksumsPath := flag.String("checksums", "", "path to checksums.txt")
	outputPath := flag.String("output", "", "formula output path")
	flag.Parse()
	if *version == "" || *checksumsPath == "" || *outputPath == "" || flag.NArg() != 0 {
		fail(errors.New("usage: homebrewformula -version vX.Y.Z -checksums checksums.txt -output onwardpg.rb"))
	}
	checksums, err := os.Open(*checksumsPath)
	if err != nil {
		fail(err)
	}
	defer checksums.Close()
	formula, err := renderFormula(*version, checksums)
	if err != nil {
		fail(err)
	}
	if err := os.WriteFile(*outputPath, formula, 0o644); err != nil {
		fail(err)
	}
}

func fail(err error) {
	_, _ = fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}

func renderFormula(version string, checksums io.Reader) ([]byte, error) {
	if !releaseVersionPattern.MatchString(version) {
		return nil, fmt.Errorf("invalid release version %q", version)
	}
	shortVersion := strings.TrimPrefix(version, "v")
	wanted := []releaseArtifact{
		{os: "darwin", arch: "amd64"},
		{os: "darwin", arch: "arm64"},
		{os: "linux", arch: "amd64"},
		{os: "linux", arch: "arm64"},
	}
	byName, err := readChecksums(checksums)
	if err != nil {
		return nil, err
	}
	for index := range wanted {
		name := artifactName(shortVersion, wanted[index].os, wanted[index].arch)
		checksum, exists := byName[name]
		if !exists {
			return nil, fmt.Errorf("checksum for %s is missing", name)
		}
		wanted[index].checksum = checksum
	}

	var formula strings.Builder
	formula.WriteString("# typed: strict\n# frozen_string_literal: true\n\n")
	formula.WriteString("# This file is generated from onwardpg release checksums. DO NOT EDIT.\n")
	formula.WriteString("class Onwardpg < Formula\n")
	formula.WriteString("  desc \"Forward-only PostgreSQL schema-diff and migration planner\"\n")
	formula.WriteString("  homepage \"https://github.com/jokull/onwardpg\"\n")
	fmt.Fprintf(&formula, "  version %q\n", shortVersion)
	formula.WriteString("  license \"MIT\"\n\n")
	writePlatform(&formula, version, shortVersion, "macos", wanted[0:2])
	formula.WriteString("\n")
	writePlatform(&formula, version, shortVersion, "linux", wanted[2:4])
	formula.WriteString("\n  test do\n")
	fmt.Fprintf(&formula, "    assert_match '\"version\":\"%s\"', shell_output(\"#{bin}/onwardpg version\")\n", version)
	formula.WriteString("  end\nend\n")
	return []byte(formula.String()), nil
}

func readChecksums(reader io.Reader) (map[string]string, error) {
	checksums := make(map[string]string)
	scanner := bufio.NewScanner(reader)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) != 2 {
			return nil, fmt.Errorf("invalid checksum line %q", scanner.Text())
		}
		decoded, err := hex.DecodeString(fields[0])
		if err != nil || len(decoded) != 32 {
			return nil, fmt.Errorf("invalid SHA-256 checksum %q", fields[0])
		}
		checksums[filepath.Base(strings.TrimPrefix(fields[1], "*"))] = fields[0]
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return checksums, nil
}

func writePlatform(formula *strings.Builder, tag, version, platform string, artifacts []releaseArtifact) {
	fmt.Fprintf(formula, "  on_%s do\n", platform)
	for _, artifact := range artifacts {
		cpu := "intel"
		if artifact.arch == "arm64" {
			cpu = "arm"
		}
		name := artifactName(version, artifact.os, artifact.arch)
		condition := "Hardware::CPU." + cpu + "?"
		if platform == "linux" {
			condition += " && Hardware::CPU.is_64_bit?"
		}
		fmt.Fprintf(formula, "    if %s\n", condition)
		fmt.Fprintf(formula, "      url \"https://github.com/jokull/onwardpg/releases/download/%s/%s\"\n", tag, name)
		fmt.Fprintf(formula, "      sha256 %q\n", artifact.checksum)
		formula.WriteString("      define_method(:install) do\n")
		fmt.Fprintf(formula, "        bin.install %q => \"onwardpg\"\n", strings.TrimSuffix(name, ".tar.gz")+"/onwardpg")
		formula.WriteString("      end\n")
		formula.WriteString("    end\n")
	}
	formula.WriteString("  end\n")
}

func artifactName(version, osName, arch string) string {
	return fmt.Sprintf("onwardpg_%s_%s_%s.tar.gz", version, osName, arch)
}
