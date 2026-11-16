# Drift

This execution's behaviour changed against what the project usually does, in a way that has no more specific playbook. Mesedi's drift checks cover a model the project has not used before (`new_model:` signatures) and a lasting change in the wording of outputs (`lexical_drift_` signatures); the signature on this group names what changed.

## What to do

1. Read the signature on the failure group: it names the model or the measure that moved.
2. Find when the change began, and what shipped then: a deploy, a prompt edit, a provider model update, or a configuration change.
3. Compare a few executions from before and after the change. Drift is a change, not necessarily a fault; decide whether the new behaviour is what you intended.
4. If the change was intended, resolve the group so new drift stands out.
