"use client";

import { useEffect, useState } from "react";
import { api, ApiError, type Stats } from "@/lib/api";

type LoadState =
  | { status: "loading" }
  | { status: "ok"; stats: Stats }
  | { status: "error"; message: string };

export default function OverviewPage() {
  const [state, setState] = useState<LoadState>({ status: "loading" });

  useEffect(() => {
    let cancelled = false;
    api
      .getStats()
      .then((stats) => {
        if (!cancelled) setState({ status: "ok", stats });
      })
      .catch((err: unknown) => {
        if (cancelled) return;
        const message =
          err instanceof ApiError
            ? `${err.status} ${err.message}`
            : err instanceof Error
              ? err.message
              : "Unknown error";
        setState({ status: "error", message });
      });
    return () => {
      cancelled = true;
    };
  }, []);

  return (
    <div className="p-8 max-w-5xl mx-auto">
      <div className="mb-10">
        <h2 className="text-2xl font-medium mb-2" style={{ color: "var(--text)" }}>
          Welcome to Mesedi
        </h2>
        <p className="text-sm" style={{ color: "var(--text-muted)" }}>
          Connect the SDK to start sending agent telemetry — the dashboard
          populates automatically.
        </p>
      </div>

      <div
        className="rounded-lg p-6 mb-8"
        style={{ background: "var(--surface)", border: "1px solid var(--border)" }}
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
export MESEDI_API_KEY=mesedi_sk_...

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
export MESEDI_API_KEY=mesedi_sk_...

import { wrap } from "mesedi";

const myAgent = wrap(async (query) => { ... });`}</code>
            </pre>
          </div>
        </div>
      </div>

      <h3 className="text-sm font-medium mb-3" style={{ color: "var(--text)" }}>
        Live stats
      </h3>

      {state.status === "error" && (
        <div
          className="rounded-md px-4 py-3 mb-4 text-xs"
          style={{
            background: "rgba(239, 68, 68, 0.08)",
            border: "1px solid var(--danger)",
            color: "var(--danger)",
          }}
        >
          Could not load stats from the backend — {state.message}
        </div>
      )}

      <div className="grid grid-cols-1 sm:grid-cols-3 gap-3">
        {[
          {
            icon: "ti-alert-triangle",
            label: "Failure groups",
            sub: "currently open",
            value:
              state.status === "ok" ? state.stats.open_failure_groups : null,
          },
          {
            icon: "ti-list-tree",
            label: "Executions",
            sub: "all time",
            value:
              state.status === "ok" ? state.stats.total_executions : null,
          },
          {
            icon: "ti-skull",
            label: "Crashes",
            sub: "last 24h",
            value: state.status === "ok" ? state.stats.crashed_24h : null,
          },
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
              style={{
                color: stat.value === null ? "var(--text-dim)" : "var(--text)",
                fontFamily: "var(--font-mono)",
              }}
            >
              {stat.value === null ? "—" : stat.value.toLocaleString()}
            </div>
            <div className="text-[11px] mt-1" style={{ color: "var(--text-dim)" }}>
              {stat.sub}
            </div>
          </div>
        ))}
      </div>
    </div>
  );
}
