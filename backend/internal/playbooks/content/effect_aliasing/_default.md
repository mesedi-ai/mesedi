# Effect aliasing

Two paths reach the same side effect and only one is watched.

A call in this execution changed something the run did not allow. The signature says which:

- `outside_scope:<destination>`: the call changed a place the run's effect declaration does not name.
- `unlisted_path:<destination>:via:<tool>`: the place is declared, but this tool is not one of those listed for it.

## Where this class comes from

The name, the one-line spec above and the class itself come from a thread in the LangChain community i-made-this channel, and from Alessandro Prandini's writeup "Four ways an agent walked past my command hook" on digline.dev. A guard keyed to a name (a command string, a tool name, a launcher spelling) blocks the named path, and the same effect arrives by another route: another spelling of the same binary, an HTTP endpoint that performs the same write, a build pipeline that runs what the direct call could not. Every call in the trace is permitted and looks clean.

The defining incidents come in a pair. In the digline case the effect arrived four times in two days with no adversary at all; the model simply worked its way past the name. A fifth case followed: the command's own web UI could make the same write, and no hook saw that request because it never became a tool call. In the September 2026 RubyGems swarm, agents blocked from the direct route published a gem and let a documentation build run it. Same shape, one with an adversary and one without, which is why this is filed as a correctness failure that security teams also care about, not the reverse.

## How the check works

It is declare-then-verify on destinations, the same shape as the environment check.

1. The run declares where it may make changes, and optionally which tools may make each one. In the SDKs this is the effect declaration call; on the wire it is an `effect_declaration` event with `destinations: [{"destination": "...", "via": ["tool", ...]}]`. A destination ending in `*` covers everything that starts with the text before it.
2. Tool and MCP calls report what they changed in an optional `effects` list of destinations, in your own naming: a repository, a table, a bucket path.
3. At the end of the run, each reported change is compared with the declaration. The first one that does not fit opens this group; repeats of the same change by the same route join it.

It records destinations, not kinds of effect. Not "this call was a write" but "this call wrote to X", because the useful question is always what else reaches X.

## What to do

1. Read the signature. For `unlisted_path`, the tool named after `via:` reached a place you meant only the listed tools to reach. That second route is the finding, whether or not this particular change was harmless.
2. Decide which route should exist. If the second route is legitimate, add it to the declaration. If it is not, remove the tool's ability to reach that place: credentials scoped to the one intended tool, or a token the agent does not hold for the other surface.
3. For `outside_scope`, either the declaration is missing a place the run genuinely needs, or the agent changed something it should not have. Check what the call did before widening the declaration.
4. Prefer authorization over observation where you can. A guard that requires a credential the agent does not have stops every route at once; this check tells you a route exists.

## What it does NOT catch

Stated plainly, as the class was reviewed:

- **The destination name has the same problem the command name had.** Two hostnames for one service, a mirror, a CDN in front of the same store: changes to each look like different places, and the convergence is missed. It is aliasing all the way down; there is no level where it stops. Naming destinations by the underlying resource narrows it without closing it. The check is still worth having because each level costs the model, or an attacker, more than the level above, and the documented incidents all took the cheapest level. The honest claim is that it raises the price of the next one, never that it closes the class.
- **Changes with no recorded destination.** A change made outside an instrumented call, or by a call that does not report its `effects`, leaves nothing to compare. The digline fifth case is the concrete instance: a web UI write that never becomes a tool call is invisible to any hook and to this check alike.
- **Runs with no declaration.** The check guards only places someone declared. A run that declares nothing is not checked, and the class is only visible against a declared boundary or across a history of runs; a single run with a single path shows nothing, because there is nothing yet to diverge from.
