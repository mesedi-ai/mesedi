# Loops

This execution tripped one of Mesedi's loop checks, under a signature with no more specific playbook. The loop checks cover the same tool called again with identical arguments (`identical_call_`), near-identical calls (`similar_call_`), a run that exceeded its time budget (`time_budget_`), and a run that exceeded its step limit (`step_count_`); the signature on this group names which check fired.

## What to do

1. Open the execution and read its last tool calls in order. A loop usually shows as the same call, or the same small cycle of calls, repeating.
2. Look at what the tool returned each time. Agents loop most often when a tool's result does not change what the model decides next: an error it does not understand, an empty result, or a result it cannot parse.
3. Give the agent a way out: a clearer error message, a limit on retries, or an instruction to stop and report when a tool keeps failing.
4. If long runs are normal for this agent, raise the step or time limit for the project rather than resolving each group.
