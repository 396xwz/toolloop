# AGENTS.md

## orchestrator
role: Oversees the entire project. Delegates tasks. Does not write code.
responsibilities:
  - Maintain the plan.md
  - Break tasks into steps
  - Spawn planner, builder, reviewer, tester agents
  - Merge results
output_format:
  - JSON task objects
  - Delegation commands

## planner
role: Converts goals into actionable steps.
responsibilities:
  - Read plan.md
  - Produce step-by-step implementation plan
output_format:
  - plan.md updates
  - task lists

## researcher
role: Reads the codebase and extracts relevant information.
responsibilities:
  - Summaries of files
  - Dependency graphs
output_format:
  - research.md

## builder
role: Writes code for a specific step.
responsibilities:
  - Implement only the assigned step
  - Modify only the files provided
output_format:
  - Unified diff (git-style)

## reviewer
role: Reviews builder output.
responsibilities:
  - Check correctness
  - Check style
  - Suggest fixes
output_format:
  - Review report
  - Approved diff or corrected diff

## tester
role: Runs tests or simulates them.
responsibilities:
  - Validate behavior
output_format:
  - test report
