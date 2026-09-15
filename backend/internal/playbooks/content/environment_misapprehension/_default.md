# Environment misapprehension

**Mesedi detects the declared-boundary half of this class, and only that half.** A detector ships with this playbook; its exact coverage and its exact blindness are both stated below, because the difference between them is the whole point of naming the class honestly.

## What the failure is

An agent, or the people operating it, holds a wrong belief about what environment it is acting in. The defining public incident: Anthropic's September 2026 report that an early Claude Opus 4.6 checkpoint breached a real third-party machine in January while treating the situation as a simulation, disregarding evidence it was on the live internet. The breach was found in August, by rescanning 481 million transcripts, an eight-month detection gap closed only because someone decided to look.

The failure has two directions. A run believed to be contained (an eval, a simulation, a staging exercise) acts on the real world; that is the direction that does damage. Or a production run behaves as if consequences are not real, hedging, fabricating, or treating live actions as rehearsal. Both are beliefs about context, and beliefs are not events.

## What the detector covers

The damaging direction, and only it: the operator declares the run's mode near start (`emit_environment_declaration("simulation")`, well-known modes "live", "simulation", "staging"), the host or sandbox reports outbound contacts as egress events, and a run declared NOT-live that reached a live-looking destination clusters under `env_mismatch:<mode>` at Critical severity. Live-looking is judged conservatively in both directions: loopback, private and link-local addresses, .local/.internal/.test names, and bare service shortnames all read as not-live, so a well-built simulation full of stubs cannot fire; public IPs and dotted public hostnames read as live. This check would have fired on the January incident, provided the harness had declared itself a simulation.

## What the detector is blind to

Everything about beliefs. It says nothing about what the model believed, only that the declared boundary was crossed in fact. A run declared "live" can never fire here, whatever the model thought it was doing; a run with no declaration has no boundary to violate; and an outbound contact the host never reported as an egress event does not exist to this detector. Mesedi still deliberately ingests no reasoning text, and the chain-of-thought monitorability a belief-reading detector would depend on is reported to be declining.

## What Mesedi DOES catch nearby

- **Model drift** (`drift`, `new_model:` signatures) fires on any model name the project has not seen recently. An early-checkpoint identifier leaking into the model field of an llm_call fires today, with no changes.
- **Sandbox escape** pattern-matches tool arguments and returns for containment probing, including cloud metadata endpoints and host paths, when the agent actively tests its boundary.
- **Record integrity** fires when the event stream itself is internally inconsistent, which is what makes a months-later rescan of the record trustworthy at all.

## What to do as an operator

Declare the mode in every harness and eval (`emit_environment_declaration`), and report egress from whatever opens the connections; the detector is only as good as what the host tells it. Keep evaluation credentials and production credentials strictly separate, so a confused run fails to authenticate rather than succeeding against the real world. And treat a firing here as an incident with the same weight as an escape attempt, which is why it defaults to Critical: from the third party's side they are the same event.

## Status

Named 2026-09-14 from the weekly incident radar, verified against every detector's source before writing; the declared-boundary detector shipped 2026-09-15 with the egress event type. Absence of an alert still says nothing about the belief half of this class, and this page will keep saying so.
