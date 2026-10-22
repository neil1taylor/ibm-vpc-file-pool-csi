---
name: test
description: Run Go unit tests with race detector. Pass a package path or test name to scope the run.
argument-hint: "[package] [test-name]"
allowed-tools: Bash(go test *), Bash(make *)
---

Run unit tests for this project using the conventions from CLAUDE.md.

## Arguments

- No arguments: run `make test` (all tests)
- One argument (package path): run `go test ./$ARGUMENTS[0]/ -v -race -count=1`
- Two arguments (package + test name): run `go test ./$ARGUMENTS[0]/ -v -race -run $ARGUMENTS[1]`

## Examples

- `/test` → `make test`
- `/test pkg/pool` → `go test ./pkg/pool/ -v -race -count=1`
- `/test pkg/pool TestPoolManager_Allocate` → `go test ./pkg/pool/ -v -race -run TestPoolManager_Allocate`
- `/test pkg/driver` → `go test ./pkg/driver/ -v -race -count=1`

## After running

- Report pass/fail count
- If any tests failed, show the failure output and suggest fixes
- If all passed, show coverage percentage if available
