---
description: Plan -> Code -> Verify -> Review -> Fix, serially, one local model process at a time (planner+coder Qwen3.8 Flash Next, reviewer DeepSeek Flash Vision, verifier = tools/verify.sh)
---
Use the subagent tool with the `chain` parameter, and NEVER the `tasks`
(parallel) parameter: only one child may run at a time on this machine.
Execute this workflow for the task: $@

Make ONE subagent call with this chain, passing output between steps via
{previous}:

1. agent "planner", task: Write plan.md for this task: $@
2. agent "coder", task: Implement plan.md, run the verifier, report. The plan is: {previous}
3. agent "reviewer", task: Review the working-tree diff against plan.md and verify.log. The coder reported: {previous}
4. agent "coder", task: Apply this review to the code (Critical items are mandatory), re-run the verifier, report. The review is: {previous}

When the chain finishes, print the final step's output verbatim, then state on
one line whether verify.log is PASS or FAIL. Do not start any other subagent.
If the chain fails at a step, report which step failed and stop.
