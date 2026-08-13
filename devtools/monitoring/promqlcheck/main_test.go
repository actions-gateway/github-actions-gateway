package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A minimal shipped/documented/runbook triple that all three checks pass. Each
// test mutates one of the three and asserts the finding, so every case starts
// from a green baseline — a check that stopped firing would otherwise be
// indistinguishable from a fixture that never tripped it.
const (
	goodRule = `apiVersion: monitoring.coreos.com/v1
kind: PrometheusRule
spec:
  groups:
    - name: actions-gateway
      rules:
        - alert: ActionsGatewayThing
          expr: |
            up == 0
          labels:
            severity: warning
          annotations:
            runbook_url: "https://example.test/operations/runbook/#actionsgatewaything"
            summary: "thing"
`
	goodDoc = "text\n\n```yaml\ngroups:\n  - name: actions-gateway\n    rules:\n\n" +
		"      - alert: ActionsGatewayThing\n" +
		"        expr: |\n          up == 0\n" +
		"        labels:\n          severity: warning\n" +
		"        annotations:\n" +
		"          runbook_url: \"https://example.test/operations/runbook/#actionsgatewaything\"\n" +
		"          summary: \"thing\"\n```\n"
	goodRunbook = "# Runbook\n\n## Alert Rule Reference\n\n### ActionsGatewayThing\n\n**Ticket.** Thing.\n"
)

func write(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return p
}

// runCase writes the three inputs and returns the findings.
func runCase(t *testing.T, ruleBody, docBody, runbookBody string) []string {
	t.Helper()
	dir := t.TempDir()
	rule := write(t, dir, "rule.yaml", ruleBody)
	doc := write(t, dir, "doc.md", docBody)
	runbook := write(t, dir, "runbook.md", runbookBody)
	findings, err := run(rule, doc, runbook)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	return findings
}

func TestBaselineIsClean(t *testing.T) {
	if f := runCase(t, goodRule, goodDoc, goodRunbook); len(f) != 0 {
		t.Fatalf("baseline should be clean, got %v", f)
	}
}

func TestFindings(t *testing.T) {
	tests := []struct {
		name    string
		rule    string
		doc     string
		runbook string
		want    string
	}{
		{
			name: "expr does not parse",
			rule: strings.Replace(goodRule, "up == 0", "up ==", 1),
			doc:  strings.Replace(goodDoc, "up == 0", "up ==", 1),
			want: "expr does not parse",
		},
		{
			// Q818's first half: a rule the docs describe that never shipped.
			name: "documented but not shipped",
			rule: strings.ReplaceAll(goodRule, "ActionsGatewayThing", "ActionsGatewayOther"),
			want: "is documented but not shipped",
		},
		{
			name: "shipped but not documented",
			doc:  strings.ReplaceAll(goodDoc, "ActionsGatewayThing", "ActionsGatewayOther"),
			want: "is shipped but not reproduced",
		},
		{
			// Q818's second half: same rule, drifted body.
			name: "summary drifted",
			doc:  strings.Replace(goodDoc, `summary: "thing"`, `summary: "other"`, 1),
			want: "has drifted from",
		},
		{
			name: "no runbook_url",
			rule: strings.Replace(goodRule, `            runbook_url: "https://example.test/operations/runbook/#actionsgatewaything"`+"\n", "", 1),
			doc:  strings.Replace(goodDoc, `          runbook_url: "https://example.test/operations/runbook/#actionsgatewaything"`+"\n", "", 1),
			want: "has no runbook_url annotation",
		},
		{
			name:    "runbook heading missing",
			runbook: "# Runbook\n\n## Alert Rule Reference\n",
			want:    "has no `### ActionsGatewayThing` entry",
		},
		{
			name:    "runbook documents an alert that does not ship",
			runbook: goodRunbook + "\n### ActionsGatewayGhost\n\n**Ticket.** Nothing ships this.\n",
			want:    "documents an alert that",
		},
		{
			name: "runbook_url anchor does not match the alert name",
			rule: strings.Replace(goodRule, "#actionsgatewaything", "#actionsgatewaysomethingelse", 1),
			doc:  strings.Replace(goodDoc, "#actionsgatewaything", "#actionsgatewaysomethingelse", 1),
			want: "does not end in #actionsgatewaything",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rule, doc, runbook := tc.rule, tc.doc, tc.runbook
			if rule == "" {
				rule = goodRule
			}
			if doc == "" {
				doc = goodDoc
			}
			if runbook == "" {
				runbook = goodRunbook
			}
			findings := runCase(t, rule, doc, runbook)
			for _, f := range findings {
				if strings.Contains(f, tc.want) {
					return
				}
			}
			t.Fatalf("no finding contained %q; got %v", tc.want, findings)
		})
	}
}

// A recording rule has no alert name and no runbook entry, so the runbook check
// must skip it rather than demand one.
func TestRecordingRuleNeedsNoRunbook(t *testing.T) {
	rule := `apiVersion: monitoring.coreos.com/v1
kind: PrometheusRule
spec:
  groups:
    - name: actions-gateway-slos
      rules:
        - record: actions_gateway:thing:rate5m
          expr: |
            rate(thing_total[5m])
`
	doc := "```yaml\ngroups:\n  - name: actions-gateway-slos\n    rules:\n\n" +
		"      - record: actions_gateway:thing:rate5m\n" +
		"        expr: |\n          rate(thing_total[5m])\n```\n"
	if f := runCase(t, rule, doc, "# Runbook\n"); len(f) != 0 {
		t.Fatalf("recording rule should need no runbook entry, got %v", f)
	}
}

// A fenced yaml block that is not a rule set (a config example) must not be
// read as one, or every doc gains a phantom empty group.
func TestNonRuleYAMLBlockIsSkipped(t *testing.T) {
	doc := goodDoc + "\n```yaml\nsomeConfig:\n  enabled: true\n```\n"
	if f := runCase(t, goodRule, doc, goodRunbook); len(f) != 0 {
		t.Fatalf("config example should be ignored, got %v", f)
	}
}
