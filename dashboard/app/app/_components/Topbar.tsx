"use client";

import { usePathname } from "next/navigation";

const SECTION_TITLES: Record<string, string> = {
  "/app": "Overview",
  "/app/failure-groups": "Failure groups",
  "/app/executions": "Executions",
  "/app/webhooks": "Webhooks",
  "/app/playbooks": "Playbooks",
  "/app/api-keys": "API keys",
  "/app/settings": "Settings",
};

export default function Topbar() {
  const pathname = usePathname();

  let title = "Dashboard";
  for (const [prefix, label] of Object.entries(SECTION_TITLES)) {
    if (prefix === "/app" ? pathname === "/app" : pathname.startsWith(prefix)) {
      title = label;
    }
  }

  return (
    <header
      className="flex items-center justify-between px-6"
      style={{
        height: "56px",
        borderBottom: "1px solid var(--border)",
        background: "var(--bg)",
      }}
    >
      <h1 className="text-base font-medium" style={{ color: "var(--text)" }}>
        {title}
      </h1>

      <div className="flex items-center gap-4">
        <div
          className="inline-flex items-center gap-2 px-2.5 py-1 rounded-full"
          style={{ background: "var(--surface)", border: "1px solid var(--border)" }}
        >
          <span
            className="w-1.5 h-1.5 rounded-full"
            style={{ background: "var(--success)" }}
          />
          <span
            className="text-[11px]"
            style={{ color: "var(--text-muted)", fontFamily: "var(--font-mono)" }}
          >
            connected
          </span>
        </div>

        <button
          className="w-8 h-8 rounded-md flex items-center justify-center transition-colors"
          style={{ background: "transparent", color: "var(--text-muted)" }}
          aria-label="Search"
        >
          <i className="ti ti-search" style={{ fontSize: "16px" }} aria-hidden="true" />
        </button>

        <button
          className="w-8 h-8 rounded-md flex items-center justify-center transition-colors"
          style={{ background: "transparent", color: "var(--text-muted)" }}
          aria-label="Notifications"
        >
          <i className="ti ti-bell" style={{ fontSize: "16px" }} aria-hidden="true" />
        </button>
      </div>
    </header>
  );
}
