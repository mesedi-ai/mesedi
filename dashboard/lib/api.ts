// Typed API client for the Mesedi backend.
//
// All requests automatically attach the bearer token from localStorage
// and the X-Mesedi-Schema-Version header. Base URL comes from the
// NEXT_PUBLIC_MESEDI_API_URL build-time env var; defaults to localhost
// for `next dev` against a local backend.

import { getApiKey } from "./auth";

const API_URL =
  process.env.NEXT_PUBLIC_MESEDI_API_URL || "http://localhost:8080";

export class ApiError extends Error {
  constructor(public status: number, message: string) {
    super(message);
    this.name = "ApiError";
  }
}

type RequestOptions = {
  method?: string;
  body?: unknown;
  apiKeyOverride?: string;
};

async function request<T>(path: string, opts: RequestOptions = {}): Promise<T> {
  const key = opts.apiKeyOverride ?? getApiKey();
  const headers = new Headers();
  if (key) {
    headers.set("Authorization", `Bearer ${key}`);
  }
  headers.set("X-Mesedi-Schema-Version", "1");
  if (opts.body !== undefined) {
    headers.set("Content-Type", "application/json");
  }

  const res = await fetch(`${API_URL}${path}`, {
    method: opts.method ?? "GET",
    headers,
    body: opts.body !== undefined ? JSON.stringify(opts.body) : undefined,
  });

  if (!res.ok) {
    let msg = `${res.status} ${res.statusText}`;
    try {
      const data = (await res.json()) as { error?: string };
      if (data?.error) msg = data.error;
    } catch {
      // body wasn't JSON; keep the status-text message
    }
    throw new ApiError(res.status, msg);
  }

  return (await res.json()) as T;
}

export interface Stats {
  ok: boolean;
  total_executions: number;
  completed_executions: number;
  crashed_24h: number;
  open_failure_groups: number;
}

export const api = {
  getStats: () => request<Stats>("/stats"),
  validateKey: (key: string) => request<Stats>("/stats", { apiKeyOverride: key }),
};

export const apiBaseUrl = API_URL;
