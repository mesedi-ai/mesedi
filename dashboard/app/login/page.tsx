"use client";

import { useState } from "react";
import { useRouter } from "next/navigation";
import { api, ApiError } from "@/lib/api";
import { setApiKey } from "@/lib/auth";

export default function LoginPage() {
  const router = useRouter();
  const [key, setKey] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [isValidating, setIsValidating] = useState(false);

  async function handleSubmit(e: React.FormEvent) {
    e.preventDefault();
    setError(null);

    const trimmed = key.trim();
    if (!trimmed) {
      setError("Enter your API key to continue.");
      return;
    }
    if (!trimmed.startsWith("mesedi_sk_")) {
      setError("Mesedi API keys start with 'mesedi_sk_'. Double-check what you pasted.");
      return;
    }

    setIsValidating(true);
    try {
      // Validate the key by calling /stats with it. If it succeeds, the
      // key is valid AND has at least read access to this project.
      await api.validateKey(trimmed);
      setApiKey(trimmed);
      router.push("/app");
    } catch (err) {
      if (err instanceof ApiError) {
        if (err.status === 401) {
          setError("That key was rejected. Check it's not revoked or from another project.");
        } else if (err.status === 0 || err.message.toLowerCase().includes("failed to fetch")) {
          setError("Could not reach the Mesedi backend. Check NEXT_PUBLIC_MESEDI_API_URL.");
        } else {
          setError(`Backend returned ${err.status}: ${err.message}`);
        }
      } else {
        setError(
          err instanceof Error ? err.message : "Unknown error contacting backend.",
        );
      }
      setIsValidating(false);
    }
  }

  return (
    <div
      className="min-h-screen flex items-center justify-center px-6"
      style={{ background: "var(--bg)" }}
    >
      <div className="w-full max-w-md">
        <div className="mb-8 text-center">
          <a href="/" className="inline-block">
            <span
              className="text-3xl font-medium tracking-tight"
              style={{ color: "var(--accent)" }}
            >
              mesedi
            </span>
          </a>
          <p
            className="text-xs tracking-wider uppercase mt-1"
            style={{ color: "var(--text-dim)", fontFamily: "var(--font-mono)" }}
          >
            dashboard sign-in
          </p>
        </div>

        <form
          onSubmit={handleSubmit}
          className="rounded-lg p-6"
          style={{
            background: "var(--surface)",
            border: "1px solid var(--border)",
          }}
        >
          <label
            htmlFor="api-key"
            className="block text-sm font-medium mb-2"
            style={{ color: "var(--text)" }}
          >
            API key
          </label>
          <input
            id="api-key"
            type="password"
            autoFocus
            autoComplete="off"
            spellCheck={false}
            placeholder="mesedi_sk_..."
            value={key}
            onChange={(e) => setKey(e.target.value)}
            disabled={isValidating}
            className="w-full px-3 py-2 rounded-md text-sm outline-none transition-colors"
            style={{
              background: "var(--bg)",
              border: "1px solid var(--border-strong)",
              color: "var(--text)",
              fontFamily: "var(--font-mono)",
            }}
          />
          <p
            className="text-[11px] mt-2"
            style={{ color: "var(--text-dim)" }}
          >
            Create or rotate keys with the backend CLI, or paste a key you
            already have. Stored in your browser only; never sent anywhere
            except the Mesedi backend.
          </p>

          {error && (
            <div
              className="mt-4 rounded-md px-3 py-2 text-xs"
              style={{
                background: "rgba(239, 68, 68, 0.08)",
                border: "1px solid var(--danger)",
                color: "var(--danger)",
              }}
            >
              {error}
            </div>
          )}

          <button
            type="submit"
            disabled={isValidating}
            className="w-full mt-5 h-10 rounded-md font-medium text-sm transition-opacity"
            style={{
              background: "var(--accent)",
              color: "var(--bg)",
              opacity: isValidating ? 0.6 : 1,
              cursor: isValidating ? "wait" : "pointer",
            }}
          >
            {isValidating ? "Validating…" : "Sign in"}
          </button>
        </form>

        <p className="text-center text-xs mt-6" style={{ color: "var(--text-dim)" }}>
          <a href="/" className="hover:opacity-80 transition-opacity">
            ← back to mesedi.ai
          </a>
        </p>
      </div>
    </div>
  );
}
