//go:build tools
// +build tools

/*
Copyright 2019 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// main is the main package for the release notes generator.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
)

/*
This tool prints all the titles of all PRs from previous release to HEAD.
This needs to be run *before* a tag is created.
Use these as the base of your release notes.
*/

const (
	// Future options for release notes
	// features      = ":sparkles: New Features"
	// bugs          = ":bug: Bug Fixes"
	// warning       = ":warning: Breaking Changes"
	unknown = ":question: Sort these by hand"
)

var (
	outputOrder = []string{
		// warning,
		// features,
		// bugs,
		unknown,
	}

	fromTag = flag.String("from", "", "The tag or commit to start from.")

	tagRegex = regexp.MustCompile(`^\[release-[\w-\.]*\]`)
)

func main() {
	flag.Parse()
	os.Exit(run())
}

func lastTag() string {
	if fromTag != nil && *fromTag != "" {
		return *fromTag
	}
	cmd := exec.Command("git", "describe", "--tags", "--abbrev=0")
	out, err := cmd.Output()
	if err != nil {
		return firstCommit()
	}
	return string(bytes.TrimSpace(out))
}

func firstCommit() string {
	cmd := exec.Command("git", "rev-list", "--max-parents=0", "HEAD")
	out, err := cmd.Output()
	if err != nil {
		return "UNKNOWN"
	}
	return string(bytes.TrimSpace(out))
}

func run() int {
	lastTag := lastTag()
	cmd := exec.Command("git", "rev-list", lastTag+"..HEAD", "--merges", "--pretty=format:%B") //nolint:gosec

	merges := map[string][]string{
		unknown: {},
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		fmt.Println("Error")
		fmt.Println(string(out))
		return 1
	}

	commits := []*commit{}
	outLines := strings.Split(string(out), "\n")
	for _, line := range outLines {
		line = strings.TrimSpace(line)
		last := len(commits) - 1
		switch {
		case strings.HasPrefix(line, "commit"):
			commits = append(commits, &commit{})
		case strings.HasPrefix(line, "Merge"):
			commits[last].merge = line
			continue
		case line == "":
		default:
			commits[last].body = line
		}
	}

	for _, c := range commits {
		body := trimTitle(c.body)
		var key, prNumber, fork string

		switch {
		// case strings.HasPrefix(body, ":sparkles:"), strings.HasPrefix(body, "✨"):
		// 	key = features
		// 	body = strings.TrimPrefix(body, ":sparkles:")
		// 	body = strings.TrimPrefix(body, "✨")
		// case strings.HasPrefix(body, ":bug:"), strings.HasPrefix(body, "🐛"):
		// 	key = bugs
		// 	body = strings.TrimPrefix(body, ":bug:")
		// 	body = strings.TrimPrefix(body, "🐛")
		// case strings.HasPrefix(body, ":warning:"), strings.HasPrefix(body, "⚠️"):
		// 	key = warning
		// 	body = strings.TrimPrefix(body, ":warning:")
		// 	body = strings.TrimPrefix(body, "⚠️")
		default:
			key = unknown
		}

		body = strings.TrimSpace(body)
		if body == "" {
			continue
		}
		body = fmt.Sprintf("- %s", body)
		_, _ = fmt.Sscanf(c.merge, "Merge pull request %s from %s", &prNumber, &fork)

		merges[key] = append(merges[key], formatMerge(body, prNumber))
	}

	// TODO Turn this into a link (requires knowing the project name + organization)
	fmt.Printf("Changes since %v\n---\n", lastTag)

	for _, key := range outputOrder {
		mergeslice := merges[key]
		if len(mergeslice) == 0 {
			continue
		}

		fmt.Println("## " + key)
		for _, merge := range mergeslice {
			fmt.Println(merge)
		}
		fmt.Println()
	}

	fmt.Println("")

	return 0
}

func trimTitle(title string) string {
	// Remove a tag prefix if found.
	title = tagRegex.ReplaceAllString(title, "")

	return strings.TrimSpace(title)
}

type commit struct {
	merge string
	body  string
}

func formatMerge(line, prNumber string) string {
	if prNumber == "" {
		return line
	}
	return fmt.Sprintf("%s (%s)", line, prNumber)
}
