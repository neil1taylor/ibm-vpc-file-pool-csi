---
name: docs-audit
description: Audit documentation for accuracy against the current codebase. Use after code changes that affect metrics, CRDs, CLI flags, Helm values, or features to ensure docs stay current.
allowed-tools: Read, Grep, Glob, Bash(go doc *)
---

Audit all markdown documentation against the current codebase for accuracy.

## Checks to Perform

### 1. Metrics (MONITORING.md vs pkg/metrics/metrics.go)

- Read `pkg/metrics/metrics.go` and extract every registered metric name, type, and labels
- Compare against the metric reference table in `MONITORING.md`
- Flag: missing metrics, wrong types, wrong labels, stale metrics that no longer exist in code

### 2. CRD Fields (CRD-SPEC.md vs api/v1alpha1/)

- Read all `*_types.go` files in `api/v1alpha1/`
- Compare struct fields against `CRD-SPEC.md`
- Flag: undocumented fields, documented fields that no longer exist, wrong types

### 3. CLI Flags (INSTALL.md / HELM-VALUES.md vs cmd/main.go)

- Read `cmd/main.go` for all flag definitions
- Compare against `INSTALL.md` and `HELM-VALUES.md`
- Flag: undocumented flags, documented flags that no longer exist

### 4. Helm Values (HELM-VALUES.md vs charts/ibm-vpc-file-pool-csi/values.yaml)

- Read `charts/ibm-vpc-file-pool-csi/values.yaml`
- Compare every key against `HELM-VALUES.md`
- Flag: undocumented values, documented values that no longer exist, wrong defaults

### 5. Cross-Reference Links

- Scan all `.md` files for markdown links like `[text](FILE.md)` and `[text](FILE.md#anchor)`
- Verify each linked file exists
- Flag: broken links to files that don't exist

### 6. Version References (UPGRADE-GUIDE.md vs CHANGELOG.md)

- Read `CHANGELOG.md` for all version headers
- Verify each version has a corresponding section in `UPGRADE-GUIDE.md`
- Flag: versions in CHANGELOG missing from UPGRADE-GUIDE

### 7. Glossary Completeness (README.md glossary)

- Scan all `.md` files for terms that appear in backticks or bold that look domain-specific
- Compare against the glossary in `README.md`
- Flag: terms used across multiple docs that aren't in the glossary

### 8. Feature Accuracy (FEATURES.md vs code)

- Check maturity badges (GA, Beta, Planned) against what's actually implemented
- Look for features described as "Planned" that now have code
- Flag: stale maturity badges

## Output

Present a summary table:

| Area | Status | Issues |
|------|--------|--------|
| Metrics | OK / X issues | list |
| CRD Fields | OK / X issues | list |
| CLI Flags | OK / X issues | list |
| Helm Values | OK / X issues | list |
| Cross-Ref Links | OK / X issues | list |
| Version Coverage | OK / X issues | list |
| Glossary | OK / X issues | list |
| Feature Badges | OK / X issues | list |

Then list each issue with:
- What's wrong
- Which file(s) to fix
- Suggested fix

Do NOT make changes — only report findings. The user decides what to fix.
