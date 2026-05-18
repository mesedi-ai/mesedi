import Image from "next/image";

const FAILURE_CLASSES = [
  "crashes",
  "time_budget",
  "step_count",
  "cost_velocity",
  "tool_failures",
  "validator_failures",
  "prompt_injection",
  "drift",
];

export default function Home() {
  return (
    <div className="min-h-screen flex flex-col" style={{ background: "var(--bg)", color: "var(--text)" }}>
      <header
        className="px-6 sm:px-10 py-4 flex items-center justify-between"
        style={{ borderBottom: "1px solid var(--border)" }}
      >
        <div className="flex items-center gap-3">
          <Image src="/mesedi-logo.png" alt="Mesedi logo" width={36} height={36} priority />
          <span className="text-lg font-medium tracking-tight" style={{ color: "var(--text)" }}>
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

      <main className="flex-1 flex flex-col items-center justify-center px-6 sm:px-10 py-16 sm:py-20">
        <div className="max-w-3xl mx-auto text-center flex flex-col items-center">
          <Image
            src="/mesedi-logo.png"
            alt="Mesedi logo"
            width={128}
            height={128}
            priority
            className="mb-8"
          />

          <div
            className="inline-flex items-center gap-2 px-3 py-1 rounded-full mb-8"
            style={{ background: "var(--surface)", border: "1px solid var(--border)" }}
          >
            <span className="w-1.5 h-1.5 rounded-full" style={{ background: "var(--success)" }}></span>
            <span
              className="text-[11px] tracking-wide"
              style={{ color: "var(--text-muted)", fontFamily: "var(--font-mono)" }}
            >
              v1 · public release
            </span>
          </div>

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

        <div className="mt-20 sm:mt-24 max-w-3xl mx-auto w-full">
          <p
            className="text-center text-xs tracking-wider uppercase mb-5"
            style={{ color: "var(--text-dim)", fontFamily: "var(--font-mono)" }}
          >
            Detects eight failure classes
          </p>
          <div className="flex flex-wrap items-center justify-center gap-2">
            {FAILURE_CLASSES.map((cls) => (
              <span
                key={cls}
                className="inline-flex items-center gap-1.5 px-3 py-1 rounded-md text-[12px]"
                style={{
                  background: "var(--surface)",
                  border: "1px solid var(--border)",
                  color: "var(--text-muted)",
                  fontFamily: "var(--font-mono)",
                }}
              >
                <span className="w-1 h-1 rounded-full" style={{ background: "var(--accent)" }}></span>
                {cls}
              </span>
            ))}
          </div>
        </div>
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
