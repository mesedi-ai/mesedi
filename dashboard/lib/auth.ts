// Local-storage-backed API-key auth.
//
// The dashboard authenticates to the Mesedi backend with a bearer token
// stored in localStorage. This is not the strongest possible scheme
// (XSS would expose it), but it is the simplest scheme that works for
// a single-user / small-team v1 dashboard. Upgrade path is HTTP-only
// cookies + Next.js API-route proxy when multi-user / team auth lands.

const STORAGE_KEY = "mesedi_api_key";

export function getApiKey(): string | null {
  if (typeof window === "undefined") return null;
  return window.localStorage.getItem(STORAGE_KEY);
}

export function setApiKey(key: string): void {
  if (typeof window === "undefined") return;
  window.localStorage.setItem(STORAGE_KEY, key);
}

export function clearApiKey(): void {
  if (typeof window === "undefined") return;
  window.localStorage.removeItem(STORAGE_KEY);
}

export function isAuthenticated(): boolean {
  return getApiKey() !== null;
}
