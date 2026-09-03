import createClient from "openapi-fetch";

import type { paths } from "./schema";

/** The base path the server injected as <base href>, without its trailing slash; "" at the web
 *  root. Derived from document.baseURI, never from location.pathname and never from a window
 *  global: doc 10 section 7.3 rule 4 forbids the inline script that would have set one. */
export function basePath(): string {
  return new URL(document.baseURI).pathname.replace(/\/$/, "");
}

/** Absolute URL for a path relative to the API root, resolved against document.baseURI.
 *  path has no leading slash: a leading slash would escape the injected <base href> and break
 *  sub-path deployment (doc 10 section 7.3 rule 5), so any stray leading slashes are stripped
 *  rather than resolved. There is no window global to read: the CSP of doc 12 section 6.6
 *  forbids the inline script that would have set one. */
export function apiUrl(path: string): string {
  return new URL(
    "api/v1/" + path.replace(/^\/+/, ""),
    document.baseURI,
  ).toString();
}

/** URL of the SSE stream, built the same way. */
export function eventsUrl(): string {
  return new URL("api/v1/events", document.baseURI).toString();
}

// The token lives in module scope only — never localStorage, a cookie or the URL — so it cannot
// leak across tabs, into history or onto the wire outside the header.
let storedCsrfToken: string | null = null;

/** The CSRF token from the last /auth/setup, /auth/login or /auth/me response. */
export function setCsrfToken(token: string | null): void {
  storedCsrfToken = token;
}

export function csrfToken(): string | null {
  return storedCsrfToken;
}

/** The one typed client. No component, hook or store may call fetch directly. */
export const api = createClient<paths>({
  baseUrl: new URL("api/v1/", document.baseURI).toString(),
  credentials: "same-origin",
});

// Doc 05 section 1.2: cookie-authenticated mutations must carry X-DLTOOL-CSRF or the server
// answers 403 /problems/csrf-token-missing; reads and tokenless requests send no header.
const CSRF_METHODS = new Set(["POST", "PUT", "PATCH", "DELETE"]);

api.use({
  onRequest({ request }) {
    const token = csrfToken();
    if (token === null || !CSRF_METHODS.has(request.method.toUpperCase())) {
      return request;
    }

    request.headers.set("X-DLTOOL-CSRF", token);
    return request;
  },
});
