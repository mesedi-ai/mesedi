export default function SettingsPage() {
  return (
    <div className="p-8 max-w-3xl mx-auto">
      <div className="mb-6">
        <h2 className="text-lg font-medium mb-1" style={{ color: "var(--text)" }}>
          Settings
        </h2>
        <p className="text-sm" style={{ color: "var(--text-muted)" }}>
          Project configuration, escalation routing, retention, and team
          membership. Sections below land in upcoming slices.
        </p>
      </div>

      <div className="space-y-3">
        {[
          {
            icon: "ti-folder",
            title: "Project",
            blurb:
              "Project name, project ID, created date, and the public stats endpoint URL.",
          },
          {
            icon: "ti-route",
            title: "Severity routing",
            blurb:
              "Which failure-class severities (critical / warning / info) fire which webhooks. Today everything fires every webhook on first occurrence.",
          },
          {
            icon: "ti-clock-hour-3",
            title: "Retention",
            blurb:
              "How long the backend keeps old executions and failure groups before pruning. Today: indefinite.",
          },
          {
            icon: "ti-users",
            title: "Team",
            blurb:
              "Invite teammates and assign read / write / admin roles. Solo-founder mode for now.",
          },
        ].map((row) => (
          <div
            key={row.title}
            className="rounded-lg p-4 flex items-start gap-4"
            style={{
              background: "var(--surface)",
              border: "1px solid var(--border)",
            }}
          >
            <div
              className="w-9 h-9 rounded-md flex items-center justify-center shrink-0"
              style={{ background: "var(--surface-2)", color: "var(--accent)" }}
            >
              <i className={`ti ${row.icon}`} style={{ fontSize: "18px" }} aria-hidden="true" />
            </div>
            <div className="flex-1 min-w-0">
              <div className="flex items-center gap-2 mb-1">
                <span className="text-sm font-medium" style={{ color: "var(--text)" }}>
                  {row.title}
                </span>
                <span
                  className="text-[10px] tracking-wider uppercase px-1.5 py-0.5 rounded"
                  style={{
                    background: "var(--surface-2)",
                    color: "var(--text-dim)",
                    fontFamily: "var(--font-mono)",
                  }}
                >
                  coming soon
                </span>
              </div>
              <p className="text-xs leading-relaxed" style={{ color: "var(--text-muted)" }}>
                {row.blurb}
              </p>
            </div>
          </div>
        ))}
      </div>
    </div>
  );
}
