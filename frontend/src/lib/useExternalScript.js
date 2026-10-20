import { useCallback, useEffect, useState } from "react";

// A third-party script that never answers must not leave a spinner up forever.
export const SCRIPT_TIMEOUT_MS = 15000;

// One in-flight or settled load per src, shared by every caller, so a remount
// (or StrictMode's double effect) never injects a second tag.
const loads = new Map();

const scriptTag = (src) => document.querySelector(`script[src="${src}"]`);

/**
 * loadExternalScript injects a <script> once and resolves true when it has
 * loaded (and `isReady`, if given, confirms the global it defines). It never
 * rejects: an error or a timeout resolves false, removes the tag and forgets
 * the load, so the next call starts a clean attempt instead of waiting on a
 * dead element.
 */
export function loadExternalScript(
  src,
  { integrity, isReady, timeoutMs = SCRIPT_TIMEOUT_MS } = {}
) {
  const cached = loads.get(src);
  if (cached && scriptTag(src)) return cached;
  if (isReady?.()) return Promise.resolve(true);

  const promise = new Promise((resolve) => {
    scriptTag(src)?.remove();
    const s = document.createElement("script");
    s.src = src;
    s.async = true;
    if (integrity) {
      s.setAttribute("integrity", integrity);
      s.setAttribute("crossorigin", "anonymous");
    }
    let timer;
    const settle = (ok) => {
      clearTimeout(timer);
      if (!ok) {
        s.remove();
        if (loads.get(src) === promise) loads.delete(src);
      }
      resolve(ok);
    };
    timer = setTimeout(() => settle(false), timeoutMs);
    s.addEventListener("load", () => settle(isReady ? !!isReady() : true));
    s.addEventListener("error", () => settle(false));
    document.body.appendChild(s);
  });
  loads.set(src, promise);
  return promise;
}

/**
 * loadStylesheet adds a <link rel="stylesheet"> once. Styling is cosmetic, so
 * nothing waits on it.
 */
export function loadStylesheet(href, { integrity } = {}) {
  if (document.querySelector(`link[href="${href}"]`)) return;
  const link = document.createElement("link");
  link.rel = "stylesheet";
  link.href = href;
  if (integrity) {
    link.setAttribute("integrity", integrity);
    link.setAttribute("crossorigin", "anonymous");
  }
  document.head.appendChild(link);
}

/**
 * useExternalScript loads `src` while `enabled` and reports
 * { status: "idle" | "loading" | "ready" | "error", retry }.
 * `retry` starts a fresh attempt after an error.
 */
export function useExternalScript(
  src,
  { integrity, isReady, timeoutMs, enabled = true } = {}
) {
  const [attempt, setAttempt] = useState(0);
  const [result, setResult] = useState({ key: null, ok: false });
  const key = `${src}#${attempt}`;

  useEffect(() => {
    if (!enabled || !src) return;
    let active = true;
    loadExternalScript(src, { integrity, isReady, timeoutMs }).then((ok) => {
      if (active) setResult({ key, ok });
    });
    return () => {
      active = false;
    };
    // isReady is a predicate the caller may recreate each render; the load is
    // keyed by src and attempt only.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [enabled, src, integrity, timeoutMs, key]);

  const retry = useCallback(() => setAttempt((a) => a + 1), []);

  let status = "idle";
  if (enabled && src) {
    if (result.key !== key) status = "loading";
    else status = result.ok ? "ready" : "error";
  }
  return { status, retry };
}
