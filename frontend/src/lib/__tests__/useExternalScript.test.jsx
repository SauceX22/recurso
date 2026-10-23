import { act, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import {
  loadExternalScript,
  loadStylesheet,
  useExternalScript,
} from "../useExternalScript";

// Each test uses its own src so the module-level cache never leaks between
// tests; only the tags a test added are removed.
let n = 0;
const nextSrc = () => `https://cdn.test/lib-${++n}.js`;
const tag = (src) => document.querySelector(`script[src="${src}"]`);

afterEach(() => {
  vi.useRealTimers();
  document.querySelectorAll('script[src^="https://cdn.test/"]').forEach((s) => s.remove());
  document.querySelectorAll('link[href^="https://cdn.test/"]').forEach((l) => l.remove());
});

describe("loadExternalScript", () => {
  it("injects one tag with SRI attributes and shares the load between callers", async () => {
    const src = nextSrc();
    const a = loadExternalScript(src, { integrity: "sha384-abc" });
    const b = loadExternalScript(src, { integrity: "sha384-abc" });
    expect(a).toBe(b);
    expect(document.querySelectorAll(`script[src="${src}"]`)).toHaveLength(1);
    expect(tag(src).getAttribute("integrity")).toBe("sha384-abc");
    expect(tag(src).getAttribute("crossorigin")).toBe("anonymous");
    tag(src).dispatchEvent(new Event("load"));
    await expect(a).resolves.toBe(true);
  });

  it("removes the tag on error so the next call starts a fresh load", async () => {
    const src = nextSrc();
    const first = loadExternalScript(src);
    tag(src).dispatchEvent(new Event("error"));
    await expect(first).resolves.toBe(false);
    expect(tag(src)).toBeNull();

    const second = loadExternalScript(src);
    expect(second).not.toBe(first);
    tag(src).dispatchEvent(new Event("load"));
    await expect(second).resolves.toBe(true);
  });

  it("gives up after the timeout instead of hanging", async () => {
    vi.useFakeTimers();
    const src = nextSrc();
    const p = loadExternalScript(src, { timeoutMs: 1000 });
    vi.advanceTimersByTime(1000);
    await expect(p).resolves.toBe(false);
    expect(tag(src)).toBeNull();
  });

  it("reports false when the script loads but its global is missing", async () => {
    const src = nextSrc();
    const p = loadExternalScript(src, { isReady: () => false });
    tag(src).dispatchEvent(new Event("load"));
    await expect(p).resolves.toBe(false);
  });

  it("skips injection when the global already exists", async () => {
    const src = nextSrc();
    await expect(loadExternalScript(src, { isReady: () => true })).resolves.toBe(true);
    expect(tag(src)).toBeNull();
  });
});

describe("loadStylesheet", () => {
  it("adds the stylesheet once with SRI attributes", () => {
    const href = "https://cdn.test/style.css";
    loadStylesheet(href, { integrity: "sha384-css" });
    loadStylesheet(href, { integrity: "sha384-css" });
    const links = document.querySelectorAll(`link[href="${href}"]`);
    expect(links).toHaveLength(1);
    expect(links[0].getAttribute("integrity")).toBe("sha384-css");
    expect(links[0].getAttribute("crossorigin")).toBe("anonymous");
  });
});

function Probe({ src, enabled = true }) {
  const { status, retry } = useExternalScript(src, { enabled });
  return (
    <div>
      <span data-testid="status">{status}</span>
      <button onClick={retry}>retry</button>
    </div>
  );
}

describe("useExternalScript", () => {
  it("is idle while disabled", () => {
    render(<Probe src={nextSrc()} enabled={false} />);
    expect(screen.getByTestId("status")).toHaveTextContent("idle");
  });

  it("goes loading → error → (retry) loading → ready", async () => {
    const src = nextSrc();
    render(<Probe src={src} />);
    expect(screen.getByTestId("status")).toHaveTextContent("loading");

    await act(async () => tag(src).dispatchEvent(new Event("error")));
    expect(screen.getByTestId("status")).toHaveTextContent("error");

    act(() => screen.getByText("retry").click());
    expect(screen.getByTestId("status")).toHaveTextContent("loading");
    expect(tag(src)).not.toBeNull();

    await act(async () => tag(src).dispatchEvent(new Event("load")));
    expect(screen.getByTestId("status")).toHaveTextContent("ready");
  });
});
