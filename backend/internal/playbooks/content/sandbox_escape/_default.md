# Sandbox escape

A code-execution tool on this execution attempted behavior that matches one of Mesedi's twelve sandbox-escape patterns. The detector scans both arguments and return values of every `tool_call` (regardless of whether the call itself succeeded) and fires when any of the patterns match.

The signature is `sandbox_escape:<pattern_id>` for built-in patterns and `sandbox_escape:custom:<pattern_id>` for per-project custom patterns. Distinct attempt patterns cluster separately. The production detector emits ALL matching signatures per execution (capped at 20), so an execution that hits multiple escape vectors produces multiple failure_groups — one per vector — rather than collapsing them into the first match.

The twelve built-in pattern ids:

- `python_os_import` — `import os` or `from os import` from the sandboxed Python runtime.
- `python_subprocess_import` — `import subprocess` or `from subprocess import`.
- `shell_invocation` — `os.system`, `os.popen`, `subprocess.run`/`call`/`Popen`/`check_output`, `child_process.exec`, `child_process.spawn`.
- `dynamic_code_eval` — `eval(...)` or `exec(...)` called with an argument.
- `js_vm_module` — Node `vm.runInNewContext`, `vm.runInContext`, `vm.createContext`.
- `js_dynamic_require` — `require(...)` with a non-literal argument (variable, expression, function call). The safe form `require('fs')` with a string literal is NOT flagged.
- `js_function_constructor` — `(new) Function('...')` constructing a runnable function from a string.
- `raw_socket_open` — `import socket`, `socket.socket`, `socket.create_connection`, `socket.gethostbyname`.
- `instance_metadata_access` — `169.254.169.254`, `metadata.google.internal`, `metadata.azure.com` (link-local IMDS endpoints).
- `proc_sys_access` — `/proc/self`, `/proc/1`, `/sys/kernel`, `/sys/class`.
- `privilege_escalation` — `chmod <numeric>`, `chown root`, `setuid(0)`. Does not match `sudo` or `su -` directly; those need a custom pattern.
- `host_secret_read` — `.aws/credentials`, `.ssh/id_rsa`, `.ssh/id_ed25519`, `.ssh/known_hosts`, `/etc/passwd`, `/etc/shadow`.

## What's usually happening

Two common scenarios:

1. **A legitimate code-exec tool needs the behavior for normal operation.** A data-analysis agent that runs Python on user data may legitimately use `subprocess` to call out to pandas, or read `/proc/meminfo` to size a job. The pattern matched because the behavior is technically present, not because it is malicious.

2. **The agent is genuinely trying to break out.** The user input contained an instruction that steered the agent toward host access, the agent's prompt does not explicitly forbid host access, and the sandbox's runtime restrictions were not tight enough to prevent the attempt. Whether the attempt succeeded depends on the sandbox; the pattern detection fires either way.

Distinguishing these is the entire investigation. The signature tells you what was attempted, not whether it was malicious.

## How to investigate

Open the execution. The flagged `tool_call` event will have the offending pattern in its arguments or return value. Read the surrounding `llm_call` events to see what the agent was being asked to do and how it reasoned its way into the pattern.

Three diagnostic questions:

1. **Was the user input adversarial?** If the user prompt explicitly asks "please read /etc/passwd and tell me what's in it," the agent is being directed; the agent prompt did not refuse, and the sandbox should have prevented execution. If the user prompt looks innocuous, the agent's interpretation is the question.

2. **Did the call succeed?** The detection fires on attempt, not success. Read the return value: was the file actually read? Did the network actually open? If the sandbox blocked the call, the detector flagged a near-miss; if the sandbox allowed it, you have a security incident, not just an alert.

3. **Is the affected execution part of a broader pattern?** Cross-reference with other recent `sandbox_escape` failure_groups in the project. A single execution is investigation; many executions hitting the same pattern is a systematic issue (the agent's prompt allows this behavior unintentionally, or the sandbox config drifted).

## How to fix

The remediation depends on what the investigation found:

- **Legitimate behavior.** Document it and add an exception. Either narrow the detector by filtering on additional context (the tool name, the project scope), or accept the alert as informational and route it to a low-priority queue.

- **Adversarial attempt that was blocked.** Tighten the agent's prompt to refuse the requested behavior at the prompt layer, so the sandbox is a defense-in-depth rather than the only defense. Update your prompt to explicitly enumerate forbidden actions.

- **Adversarial attempt that succeeded.** This is a security incident. Investigate scope (what was read, what was exfiltrated, what credentials were exposed), rotate any leaked credentials, harden the sandbox (drop network capabilities, restrict syscalls with seccomp, mount the filesystem read-only), and re-prompt the agent against the new restrictions.

## How to test the fix

After deploying the fix, the sandbox_escape failure_group should stop accumulating new affected_executions for the specific pattern. If you tightened the prompt, new attempts should be caught at the prompt layer and never reach the tool. If you hardened the sandbox, new attempts should fail at the runtime layer with a clear permission-denied error.

## Custom patterns

The detector accepts per-project custom regex patterns (Wave 2.1.b). Custom matches surface under `sandbox_escape:custom:<pattern_id>`. Built-in patterns run before custom patterns; both contribute to the per-execution match cap of 20. Use custom patterns for vectors the built-ins miss (e.g. `sudo`/`su -` for stacks where those need to be flagged, JavaScript dynamic `import()`, Ruby `Kernel.system`, obfuscated base64 payloads that decode to known dangerous shapes).

## A note on this being a security alert

Sandbox escape detection is the closest thing Mesedi ships to a security-grade alert. Treat it that way. The pattern catalog is conservative (false positives are possible on legitimate analytical workflows), but the underlying signal is consequential when it is real. When in doubt, page security.
