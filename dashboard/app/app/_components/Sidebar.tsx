"use client";

import Link from "next/link";
import { usePathname, useRouter } from "next/navigation";
import { clearApiKey } from "@/lib/auth";

type NavItem = {
  href: string;
  icon: string;
  label: string;
};

const NAV_ITEMS: NavItem[] = [
  { href: "/app", icon: "ti-layout-dashboard", label: "Overview" },
  { href: "/app/failure-groups", icon: "ti-alert-triangle", label: "Failure groups" },
  { href: "/app/executions", icon: "ti-list-tree", label: "Executions" },
  { href: "/app/webhooks", icon: "ti-webhook", label: "Webhooks" },
  { href: "/app/playbooks", icon: "ti-book", label: "Playbooks" },
  { href: "/app/api-keys", icon: "ti-key", label: "API keys" },
  { href: "/app/settings", icon: "ti-settings", label: "Settings" },
];

export default function Sidebar() {
  const pathname = usePathname();
  const router = useRouter();

  function handleSignOut() {
    clearApiKey();
    router.push("/login");
  }

  return (
    <aside
      className="flex flex-col shrink-0"
      style={{
        width: "240px",
        borderRight: "1px solid var(--border)",
        background: "var(--bg)",
      }}
    >
      <div className="px-5 py-5">
        <Link href="/app" className="block">
          <div
            className="text-2xl font-medium tracking-tight"
            style={{ color: "var(--accent)" }}
          >
            mesedi
          </div>
          <div
            className="text-[10px] tracking-wider uppercase mt-1"
            style={{ color: "var(--text-dim)", fontFamily: "var(--font-mono)" }}
          >
            dashboard
          </div>
        </Link>
      </div>

      <nav className="flex-1 px-3 pt-2 flex flex-col gap-0.5">
        {NAV_ITEMS.map((item) => {
          const isActive =
            item.href === "/app"
              ? pathname === "/app"
              : pathname === item.href || pathname.startsWith(item.href + "/");
          return (
            <Link
              key={item.href}
              href={item.href}
              className="flex items-center gap-3 px-3 py-2 rounded-md text-sm transition-colors"
              style={{
                color: isActive ? "var(--accent)" : "var(--text-muted)",
                background: isActive ? "var(--surface)" : "transparent",
                fontWeight: isActive ? 500 : 400,
              }}
            >
              <i className={`ti ${item.icon}`} style={{ fontSize: "18px" }} aria-hidden="true" />
              <span>{item.label}</span>
            </Link>
          );
        })}
      </nav>

      <div
        className="px-5 py-4 flex items-center gap-3"
        style={{ borderTop: "1px solid var(--border)" }}
      >
        <div
          className="w-8 h-8 rounded-full flex items-center justify-center text-sm font-medium"
          style={{ background: "var(--surface-2)", color: "var(--text)" }}
        >
          R
        </div>
        <div className="flex-1 min-w-0">
          <div className="text-xs truncate" style={{ color: "var(--text)" }}>
            Robert Canario
          </div>
          <div
            className="text-[10px] truncate"
            style={{ color: "var(--text-dim)", fontFamily: "var(--font-mono)" }}
          >
            local · v1
          </div>
        </div>
        <button
          onClick={handleSignOut}
          className="transition-colors"
          style={{
            background: "transparent",
            border: "none",
            padding: 0,
            color: "var(--text-dim)",
            cursor: "pointer",
          }}
          aria-label="Sign out"
          title="Sign out"
        >
          <i
            className="ti ti-logout"
            style={{ fontSize: "16px" }}
            aria-hidden="true"
          />
        </button>
      </div>
    </aside>
  );
}
