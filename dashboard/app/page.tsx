type FailureClassEntry = {
  name: string;
  icon: string;
  blurb: string;
};

const FAILURE_CLASSES: FailureClassEntry[] = [
  {
    name: "crashes",
    icon: "ti-skull",
    blurb: "Unhandled exceptions, OOMs, segfaults. The obvious failures — caught and clustered, not silently swallowed.",
  },
  {
    name: "time_budget",
    icon: "ti-clock-pause",
    blurb: "Agents that run longer than they should. Usually a stuck loop or a slow external tool.",
  },
  {
    name: "step_count",
    icon: "ti-stairs",
    blurb: "Excessive iterations. The classic sign of a loop the agent can't break out of on its own.",
  },
  {
    name: "cost_velocity",
    icon: "ti-flame",
    blurb: "Burn rate above your $/minute threshold. Catches runaway spend in seconds — not in next month's invoice.",
  },
  {
    name: "tool_failures",
    icon: "ti-tool",
    blurb: "A tool call that errored, timed out, or returned malformed output the agent then tried to interpret anyway.",
  },
  {
    name: "validator_failures",
    icon: "ti-shield-x",
    blurb: "The agent's own self-check rejected its output. Surfaces these so they don't pile up unseen.",
  },
  {
    name: "prompt_injection",
    icon: "ti-virus",
    blurb: "Suspicious tokens in user input that look designed to override the system prompt. Heuristic, not exhaustive.",
  },
  {
    name: "drift",
    icon: "ti-trending-down",
    blurb: "Output distribution shifted from baseline. Either the model changed under you, or the input mix did.",
  },
];

const STEPS = [
  {
    n: "01",
    title: "Install",
    blurb: "Add the SDK to your existing project. No infra to stand up — Mesedi runs as a hosted service.",
    python: "pip install mesedi",
    typescript: "npm install mesedi",
  },
  {
    n: "02",
    title: "Wrap your agent",
    blurb: "One decorator (Python) or one function call (Node) per agent. Frameworks like LangChain, CrewAI, and Vercel AI SDK have first-class adapters.",
    python: `from mesedi import wrap

@wrap()
def my_agent(query: str) -> str:
    # your agent code
    return result`,
    typescript: `import { wrap } from "mesedi";

export const myAgent = wrap(
  async (query: string) => {
    // your agent code
    return result;
  }
);`,
  },
  {
    n: "03",
    title: "Watch for failures",
    blurb: "Open the dashboard or wire a webhook. First time a new failure class appears, you get paged once — not every time it repeats.",
    python: "# open https://mesedi.vercel.app/app\n# or POST a webhook in Settings → Routing",
    typescript: "// open https://mesedi.vercel.app/app\n// or POST a webhook in Settings → Routing",
  },
];

export default function Home() {
  return (
    <div className="min-h-screen flex flex-col" style={{ background: "var(--bg)", color: "var(--text)" }}>
      <header
        className="px-6 sm:px-10 py-4 flex items-center justify-between"
        style={{ borderBottom: "1px solid var(--border)" }}
      >
        <div className="flex items-center gap-3">
          <span
            className="text-4xl font-bold tracking-tight"
            style={{ color: "var(--text)" }}
          >
            mesedi
          </span>
        </div>
        <nav className="flex items-center gap-6 text-sm">
          <a
            href="https://github.com/mesedi-ai/mesedi"
            target="_blank"
            rel="noopener noreferrer"
            className="transition-colors hover:opacity-80"
            style={{ color: "var(--text-muted)" }}
          >
            GitHub
          </a>
          <a
            href="/app"
            className="transition-colors hover:opacity-80"
            style={{ color: "var(--text)" }}
          >
            Open dashboard →
          </a>
        </nav>
      </header>

      <main className="flex-1 flex flex-col items-center px-6 sm:px-10 pt-6 sm:pt-10 pb-16 sm:pb-20">
        <div className="max-w-3xl mx-auto text-center flex flex-col items-center">
          <h1
            className="text-5xl sm:text-6xl font-medium tracking-tight mb-6"
            style={{ color: "var(--text)", lineHeight: 1.1 }}
          >
            Guardians for
            <br />
            <span style={{ color: "var(--accent)" }}>autonomous AI agents.</span>
          </h1>

          <p
            className="text-lg sm:text-xl max-w-2xl mx-auto leading-relaxed mb-10"
            style={{ color: "var(--text-muted)" }}
          >
            Detect failures, halt runaways, and route the first occurrence to your team — the moment it happens.
          </p>

          <div className="flex flex-col sm:flex-row gap-3 items-center justify-center">
            <a
              href="https://github.com/mesedi-ai/mesedi"
              target="_blank"
              rel="noopener noreferrer"
              className="inline-flex items-center justify-center gap-2 h-11 px-5 rounded-lg font-medium transition-colors w-full sm:w-auto hover:opacity-90"
              style={{ background: "var(--accent)", color: "var(--bg)" }}
            >
              View on GitHub →
            </a>
            <a
              href="/app"
              className="inline-flex items-center justify-center gap-2 h-11 px-5 rounded-lg transition-colors w-full sm:w-auto"
              style={{
                background: "transparent",
                border: "1px solid var(--border-strong)",
                color: "var(--text)",
              }}
            >
              Open dashboard
            </a>
          </div>
        </div>

        <section className="mt-24 sm:mt-32 max-w-5xl mx-auto w-full">
          <div className="text-center mb-10">
            <p
              className="text-xs tracking-wider uppercase mb-3"
              style={{ color: "var(--text-dim)", fontFamily: "var(--font-mono)" }}
            >
              Eight failure classes
            </p>
            <h2 className="text-3xl sm:text-4xl font-medium tracking-tight" style={{ color: "var(--text)" }}>
              Agents fail in patterns.
              <br />
              <span style={{ color: "var(--accent)" }}>Mesedi catalogs them.</span>
            </h2>
          </div>

          <div className="grid grid-cols-1 sm:grid-cols-2 gap-3">
            {FAILURE_CLASSES.map((cls) => (
              <div
                key={cls.name}
                className="rounded-lg p-4 flex items-start gap-3"
                style={{ background: "var(--surface)", border: "1px solid var(--border)" }}
              >
                <div
                  className="w-9 h-9 rounded-md flex items-center justify-center shrink-0"
                  style={{ background: "var(--surface-2)", color: "var(--accent)" }}
                >
                  <i className={`ti ${cls.icon}`} style={{ fontSize: "18px" }} aria-hidden="true" />
                </div>
                <div className="flex-1 min-w-0">
                  <div
                    className="text-[13px] mb-1"
                    style={{ color: "var(--accent)", fontFamily: "var(--font-mono)" }}
                  >
                    {cls.name}
                  </div>
                  <p className="text-xs leading-relaxed" style={{ color: "var(--text-muted)" }}>
                    {cls.blurb}
                  </p>
                </div>
              </div>
            ))}
          </div>
        </section>

        <section className="mt-24 sm:mt-32 max-w-3xl mx-auto w-full">
          <div className="text-center mb-12">
            <p
              className="text-xs tracking-wider uppercase mb-3"
              style={{ color: "var(--text-dim)", fontFamily: "var(--font-mono)" }}
            >
              How it works
            </p>
            <h2 className="text-3xl sm:text-4xl font-medium tracking-tight" style={{ color: "var(--text)" }}>
              Drop-in adoption.
              <br />
              <span style={{ color: "var(--accent)" }}>Three steps.</span>
            </h2>
          </div>

          <div className="space-y-6">
            {STEPS.map((step) => (
              <div
                key={step.n}
                className="rounded-lg p-6"
                style={{ background: "var(--surface)", border: "1px solid var(--border)" }}
              >
                <div className="flex items-baseline gap-3 mb-3">
                  <span
                    className="text-sm"
                    style={{ color: "var(--accent)", fontFamily: "var(--font-mono)" }}
                  >
                    {step.n}
                  </span>
                  <span className="text-lg font-medium" style={{ color: "var(--text)" }}>
                    {step.title}
                  </span>
                </div>
                <p className="text-sm mb-4 leading-relaxed" style={{ color: "var(--text-muted)" }}>
                  {step.blurb}
                </p>

                <div className="grid grid-cols-1 sm:grid-cols-2 gap-3">
                  <div>
                    <div
                      className="text-[10px] tracking-wider uppercase mb-2"
                      style={{ color: "var(--text-dim)", fontFamily: "var(--font-mono)" }}
                    >
                      Python
                    </div>
                    <pre
                      className="rounded-md p-3 overflow-x-auto text-[12px] leading-snug"
                      style={{
                        background: "var(--bg)",
                        border: "1px solid var(--border)",
                        color: "var(--text)",
                        fontFamily: "var(--font-mono)",
                      }}
                    >
                      <code>{step.python}</code>
                    </pre>
                  </div>
                  <div>
                    <div
                      className="text-[10px] tracking-wider uppercase mb-2"
                      style={{ color: "var(--text-dim)", fontFamily: "var(--font-mono)" }}
                    >
                      TypeScript
                    </div>
                    <pre
                      className="rounded-md p-3 overflow-x-auto text-[12px] leading-snug"
                      style={{
                        background: "var(--bg)",
                        border: "1px solid var(--border)",
                        color: "var(--text)",
                        fontFamily: "var(--font-mono)",
                      }}
                    >
                      <code>{step.typescript}</code>
                    </pre>
                  </div>
                </div>
              </div>
            ))}
          </div>
        </section>

        <section className="mt-24 sm:mt-32 max-w-3xl mx-auto w-full">
          <div className="text-center mb-8">
            <p
              className="text-xs tracking-wider uppercase mb-3"
              style={{ color: "var(--text-dim)", fontFamily: "var(--font-mono)" }}
            >
              Why this exists
            </p>
            <h2 className="text-3xl sm:text-4xl font-medium tracking-tight" style={{ color: "var(--text)" }}>
              You can&apos;t debug what you
              <br />
              <span style={{ color: "var(--accent)" }}>can&apos;t see fail.</span>
            </h2>
          </div>

          <div
            className="rounded-lg p-6 sm:p-8 space-y-4 text-sm leading-relaxed"
            style={{
              background: "var(--surface)",
              border: "1px solid var(--border)",
              color: "var(--text-muted)",
            }}
          >
            <p>
              Production AI agents fail in patterns, not in one-offs. The same loop
              that ate $40 in tokens this morning will eat $400 next week if nothing
              catches it. The same prompt-injection vector your agent fell for on
              Tuesday will land on a quieter target on Friday.
            </p>
            <p>
              The existing observability stack — LangSmith, Arize, generic APM —
              shows you traces. Mesedi is built around the assumption that what you
              actually need is a <em>cluster of related failures with a name</em>,
              fired exactly once to wherever you actually watch (Slack, PagerDuty,
              your inbox), with a canonical playbook fix attached.
            </p>
            <p>
              Eight failure classes are detected today. The product is open-source
              MIT, the SDKs ship to PyPI and npm, and the backend runs on Fly.io.
              Self-host if you want to; use the hosted service if you don&apos;t.
            </p>
          </div>
        </section>
      </main>

      <footer
        className="px-6 sm:px-10 py-4 flex flex-col sm:flex-row items-center justify-between gap-2"
        style={{ borderTop: "1px solid var(--border)" }}
      >
        <p className="text-xs" style={{ color: "var(--text-dim)" }}>
          © 2026 Mesedi · Built for production AI agents
        </p>
        <div className="flex items-center gap-5 text-xs" style={{ color: "var(--text-dim)" }}>
          <a
            href="https://github.com/mesedi-ai/mesedi"
            target="_blank"
            rel="noopener noreferrer"
            className="transition-colors hover:opacity-80"
          >
            GitHub
          </a>
          <a
            href="https://pypi.org/project/mesedi/"
            target="_blank"
            rel="noopener noreferrer"
            className="transition-colors hover:opacity-80"
          >
            PyPI
          </a>
          <a
            href="https://www.npmjs.com/package/mesedi"
            target="_blank"
            rel="noopener noreferrer"
            className="transition-colors hover:opacity-80"
          >
            npm
          </a>
        </div>
      </footer>
    </div>
  );
}
