export default function OverviewPage() {
  return (
    <div className="p-8 max-w-5xl mx-auto">
      <div className="mb-10">
        <h2 className="text-2xl font-medium mb-2" style={{ color: "var(--text)" }}>
          Welcome to Mesedi
        </h2>
        <p className="text-sm" style={{ color: "var(--text-muted)" }}>
          No data yet. Connect the SDK to start sending agent telemetry — the
          dashboard will populate automatically.
        </p>
      </div>

      <div
        className="rounded-lg p-6"
        style={{
          background: "var(--surface)",
          border: "1px solid var(--border)",
        }}
      >
        <div className="flex items-center gap-2 mb-4">
          <i
            className="ti ti-terminal-2"
            style={{ fontSize: "18px", color: "var(--accent)" }}
            aria-hidden="true"
          />
          <span className="text-sm font-medium" style={{ color: "var(--text)" }}>
            Get started
          </span>
        </div>

        <div className="space-y-4">
          <div>
            <div
              className="text-[11px] tracking-wider uppercase mb-2"
              style={{ color: "var(--text-dim)", fontFamily: "var(--font-mono)" }}
            >
              Python
            </div>
            <pre
              className="rounded-md p-3 overflow-x-auto text-[13px]"
              style={{
                background: "var(--bg)",
                border: "1px solid var(--border)",
                color: "var(--text)",
                fontFamily: "var(--font-mono)",
              }}
            >
              <code>{`pip install mesedi
export MESEDI_API_KEY=mk_...

from mesedi import wrap

@wrap()
def my_agent(query):
    ...`}</code>
            </pre>
          </div>

          <div>
            <div
              className="text-[11px] tracking-wider uppercase mb-2"
              style={{ color: "var(--text-dim)", fontFamily: "var(--font-mono)" }}
            >
              Node / TypeScript
            </div>
            <pre
              className="rounded-md p-3 overflow-x-auto text-[13px]"
              style={{
                background: "var(--bg)",
                border: "1px solid var(--border)",
                color: "var(--text)",
                fontFamily: "var(--font-mono)",
              }}
            >
              <code>{`npm install mesedi
export MESEDI_API_KEY=mk_...

import { wrap } from "mesedi";

const myAgent = wrap(async (query) => { ... });`}</code>
            </pre>
          </div>
        </div>
      </div>

      <div className="mt-8 grid grid-cols-1 sm:grid-cols-3 gap-3">
        {[
          { icon: "ti-alert-triangle", label: "Failure groups", count: "0" },
          { icon: "ti-list-tree", label: "Executions", count: "0" },
          { icon: "ti-webhook", label: "Webhooks fired", count: "0" },
        ].map((stat) => (
          <div
            key={stat.label}
            className="rounded-lg p-4"
            style={{
              background: "var(--surface)",
              border: "1px solid var(--border)",
            }}
          >
            <div className="flex items-center gap-2 mb-2">
              <i
                className={`ti ${stat.icon}`}
                style={{ fontSize: "14px", color: "var(--text-dim)" }}
                aria-hidden="true"
              />
              <span
                className="text-[11px] tracking-wider uppercase"
                style={{ color: "var(--text-dim)", fontFamily: "var(--font-mono)" }}
              >
                {stat.label}
              </span>
            </div>
            <div
              className="text-2xl font-medium"
              style={{ color: "var(--text)", fontFamily: "var(--font-mono)" }}
            >
              {stat.count}
            </div>
            <div className="text-[11px] mt-1" style={{ color: "var(--text-dim)" }}>
              last 24h
            </div>
          </div>
        ))}
      </div>
    </div>
  );
}
