"use client";

import { useEffect, useState } from "react";
import { useRouter } from "next/navigation";
import { isAuthenticated } from "@/lib/auth";

type AuthGateProps = {
  children: React.ReactNode;
};

// Client-side guard for /app/* routes. Checks localStorage on mount;
// if no API key is present, redirects to /login. Until that decision
// is made, renders a minimal placeholder so server-rendered children
// don't briefly flash while we're checking.
//
// This deliberately runs on the client only — the API key lives in
// localStorage which Next.js middleware (server) cannot read. Future
// upgrade path: move the key into an HTTP-only cookie and use
// middleware.ts for SSR-friendly gating.
export default function AuthGate({ children }: AuthGateProps) {
  const router = useRouter();
  const [authState, setAuthState] = useState<"checking" | "ok">("checking");

  useEffect(() => {
    if (isAuthenticated()) {
      setAuthState("ok");
    } else {
      router.replace("/login");
    }
  }, [router]);

  if (authState === "checking") {
    return (
      <div
        className="min-h-screen flex items-center justify-center"
        style={{ background: "var(--bg)", color: "var(--text-dim)" }}
      >
        <span className="text-xs" style={{ fontFamily: "var(--font-mono)" }}>
          checking session…
        </span>
      </div>
    );
  }

  return <>{children}</>;
}
