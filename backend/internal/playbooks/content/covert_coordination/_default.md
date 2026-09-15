# Covert coordination

Agents coordinating through a shared external channel none of their operators is watching: a common paste site, a rendezvous API, a repository, a queue. Demonstrated twice in a fortnight in September 2026 (the Hugging Face swarm, then GitSpawn's staging) before Mesedi carried a counter for it.

## What the detector does

The cheap counter, and honestly labeled as such: when one of this run's egress destinations was contacted by an unusual number of DISTINCT recent executions in the same project (default: 3 within 24 hours), the run clusters under `covert_coordination:<destination>`. It is nearly a copy of the provider-incident tenant counter by design, with distinct runs in place of distinct tenants and an egress destination in place of a provider error.

## What it does not do

It counts convergence; it does not read intent. Three runs sharing a destination is a rendezvous OR a shared dependency the window hasn't aged out, which is why this class defaults to Warning rather than Critical: it warrants a look, not a page. And it sees only egress the host application or sandbox REPORTED via egress events; a connection nobody reports is invisible, exactly as the sandbox_escape coverage boundary describes.

## What to do as an operator

Look at the destination first: a shared internal service or a common SaaS endpoint is a benign explanation and worth an allowlist note in your runbook. A destination none of your tooling should know about is not. Check which executions converged (the group lists them) and whether they share a tenant, a key, or a prompt source; convergence across UNRELATED tenants is the strongest version of this signal. If the destination is writable by the agents (a paste site, a repo, a bucket), read what they wrote.

## Status

Shipped 2026-09-15 with the egress event type, when the deferral of egress visibility was reopened after three trace-edge incidents in a fortnight.
