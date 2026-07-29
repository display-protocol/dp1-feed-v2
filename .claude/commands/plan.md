---
description: Run a planning pass for large or ambiguous work
argument-hint: "[task description]"
allowed-tools: Bash(git status:*), Bash(git log:*), Read, Grep, Glob, Agent
---

Run a planning pass for the following task using `CLAUDE.md`, `PLANS.md`, and the repository rules:

$ARGUMENTS

Delegate to the `planner-researcher` subagent (Agent tool, `subagent_type: planner-researcher`).

The plan must follow the shape in `PLANS.md`:

1. Current context — relevant packages, current behavior, operational or product constraints
2. Constraints and invariants
3. Unknowns, assumptions, and missing owner decisions (surface them, do not guess)
4. Design branches when materially different options exist
5. Tests and verification, defined before implementation details
6. Staged delivery plan with reversible increments

Do not edit files during this pass. Do not jump to implementation.
