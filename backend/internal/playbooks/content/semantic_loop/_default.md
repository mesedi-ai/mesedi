# Semantic loop

The agent revisited the same canonical state three or more times across `checkpoint` events on this execution. Mesedi's detector hashes a canonical normalization of each checkpoint's state (sorted keys, lowercased strings, rounded floats) and fires when the same hash recurs three times within one run. The surface text of each step may differ, but the underlying logical state is the same.

This is distinct from `identical_call` (same LLM prompt repeated) and `similar_call` (lexically similar LLM prompts). Semantic loops show up even when the agent is producing different prompts each time, because the agent is fundamentally doing the same work over and over.

## What's usually happening

Three common causes, in rough order of frequency:

1. **Missing progress invariant.** The agent's loop terminator depends on a condition that the agent itself does not check, so it keeps "trying" without ever advancing. Common in goal-seeking agents that don't compare current state to prior state.

2. **State-machine bug masked by output variance.** The agent rephrases each step but the underlying decision is the same. The variance comes from temperature, not progress.

3. **Genuine semantic ambiguity in the task.** The task is under-specified and the agent has no signal it's spinning. Different from a code bug; the prompt needs more constraint.

## How to investigate

Open the execution and look at the `checkpoint` events. They were the input to the detector and they share a canonical state hash. Read the state dictionaries on three consecutive checkpoints. If the values are semantically equivalent (same items, same flags, same decisions, just rephrased), the detector caught a real loop. If they look genuinely different, the canonical normalization may need tuning for your domain.

A useful debugging step: emit a `checkpoint` event at every major decision point with a state dict that captures only the fields that should change between iterations. The detector will fire faster and more accurately if it sees state that is supposed to evolve.

## How to fix

The remediation depends on the cause:

- **Missing progress invariant.** Add an explicit "did anything actually change?" check at each step. If the agent is in step N and its state matches step N-1, exit the loop or escalate (raise an error, fall back to a degraded path, request human input).

- **State-machine bug.** Find the condition that is supposed to advance the agent and verify it actually does. Common bugs: comparing the wrong fields, mutating the wrong dict, expecting the model to set a flag the prompt never asks for.

- **Under-specified task.** Tighten the prompt with a clear stopping criterion. "Continue until you have a confident answer" is not a stopping criterion; "continue at most 5 steps then return whatever you have" is.

## How to test the fix

After deploying, run the same workload and watch for new entries in the failure_group. The affected_executions count should plateau. If it continues to grow, the canonical state you are checkpointing does not match what the detector hashes; widen the checkpoint payload to include the fields that should advance.

## A note on the detection algorithm

The detector uses canonical-state hashing, not entropy. Entropy-based loop detection (variance across consecutive prompts) generates too many false positives on legitimate creative variation. Hashing a normalized state dict requires the agent to declare what state it is in via checkpoint events, which is a small SDK lift but produces much cleaner signal.
