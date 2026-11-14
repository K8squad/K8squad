// test/tickets/useTicketRunStream.test.tsx — ISI-5206 (S3 of ISI-5202).
//
// The ticket view must live-tail ANY active run, not only a run it dispatched
// itself this session. This proves:
//   • the gating rule (ambientRunId): open the stream for the holding run only
//     when it is live AND the self-dispatch ladder isn't already streaming it;
//   • a `thinking` frame on that ambient stream surfaces as a LiveThinkingRow;
//   • the run's terminal `ended` milestone fires onEnded exactly once (the caller
//     re-fetches the thread → durable tail + running state cleared, no refresh);
//   • an inert run (no id / terminal lane / self-dispatch overlap) opens nothing.

import { describe, it, expect, afterEach, vi } from "vitest";
import { renderHook, act, cleanup } from "@testing-library/react";
import {
  ambientRunId,
  useTicketRunStream,
} from "@/lib/tickets/useTicketRunStream";

// Controllable EventSource stub (jsdom has none) — captures per-name listeners so
// a test can push named SSE frames and assert what the hook accumulated.
class FakeEventSource {
  static CONNECTING = 0 as const;
  static OPEN = 1 as const;
  static CLOSED = 2 as const;
  url: string;
  onopen: (() => void) | null = null;
  onerror: (() => void) | null = null;
  onmessage: ((e: MessageEvent) => void) | null = null;
  listeners = new Map<string, (e: MessageEvent) => void>();
  closed = false;
  constructor(url: string) {
    this.url = url;
  }
  addEventListener(name: string, fn: (e: MessageEvent) => void) {
    this.listeners.set(name, fn);
  }
  removeEventListener(name: string) {
    this.listeners.delete(name);
  }
  emit(name: string, data: unknown, lastEventId = "evt-1") {
    this.listeners.get(name)?.({
      data: JSON.stringify(data),
      lastEventId,
    } as MessageEvent);
  }
  close() {
    this.closed = true;
  }
}

function stubEventSource() {
  const instances: FakeEventSource[] = [];
  const ctor = vi.fn((url: string) => {
    const es = new FakeEventSource(url);
    instances.push(es);
    return es;
  });
  Object.assign(ctor, {
    CONNECTING: FakeEventSource.CONNECTING,
    OPEN: FakeEventSource.OPEN,
    CLOSED: FakeEventSource.CLOSED,
  });
  vi.stubGlobal("EventSource", ctor as unknown as typeof EventSource);
  return instances;
}

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

describe("ambientRunId — the gating rule (ISI-5206)", () => {
  it("opens the holding run when it is live and unclaimed by the ladder", () => {
    expect(ambientRunId("run-7", "in_progress", undefined)).toBe("run-7");
    expect(ambientRunId("run-7", "in_review", "")).toBe("run-7");
  });

  it("is inert with no holding run", () => {
    expect(ambientRunId("", "in_progress", undefined)).toBe("");
  });

  it("is inert on a terminal / intake lane (nothing live to tail)", () => {
    expect(ambientRunId("run-7", "done", undefined)).toBe("");
    expect(ambientRunId("run-7", "backlog", undefined)).toBe("");
    expect(ambientRunId("run-7", "todo", undefined)).toBe("");
    expect(ambientRunId("run-7", "cancelled", undefined)).toBe("");
  });

  it("defers to the self-dispatch ladder when it already streams this run (one EventSource)", () => {
    expect(ambientRunId("run-7", "in_progress", "run-7")).toBe("");
    // a DIFFERENT self-dispatch run does not suppress the ambient one
    expect(ambientRunId("run-7", "in_progress", "run-other")).toBe("run-7");
  });
});

describe("useTicketRunStream — live tail + terminal resolve (ISI-5206)", () => {
  it("opens exactly one EventSource against the ambient run's BFF stream", () => {
    const instances = stubEventSource();
    renderHook(() =>
      useTicketRunStream("run-7", "in_progress", undefined, () => {}),
    );
    expect(instances).toHaveLength(1);
    expect(instances[0].url).toBe("/api/runs/run-7/stream");
  });

  it("opens NO stream when inert (terminal lane)", () => {
    const instances = stubEventSource();
    renderHook(() => useTicketRunStream("run-7", "done", undefined, () => {}));
    expect(instances).toHaveLength(0);
  });

  it("opens NO stream when the self-dispatch ladder already owns the run", () => {
    const instances = stubEventSource();
    renderHook(() =>
      useTicketRunStream("run-7", "in_progress", "run-7", () => {}),
    );
    expect(instances).toHaveLength(0);
  });

  it("surfaces a `thinking` frame as a LiveThinkingRow (author/body/at)", () => {
    const instances = stubEventSource();
    const { result } = renderHook(() =>
      useTicketRunStream("run-7", "in_progress", undefined, () => {}),
    );
    act(() => {
      instances[0].emit("thinking", {
        runId: "run-7",
        author: "agent:sam",
        body: "[run run-7][tool:read/start] repo.ts",
        at: "2026-09-29T10:00:00.000Z",
      });
    });
    expect(result.current).toEqual([
      {
        author: "agent:sam",
        body: "[run run-7][tool:read/start] repo.ts",
        at: "2026-09-29T10:00:00.000Z",
      },
    ]);
  });

  it("carries the ISI-7098 tier through: a 'deliverable' frame is the clean-comment kind (ISI-7101)", () => {
    const instances = stubEventSource();
    const { result } = renderHook(() =>
      useTicketRunStream("run-7", "in_progress", undefined, () => {}),
    );
    act(() => {
      instances[0].emit("thinking", {
        runId: "run-7",
        author: "agent:sam",
        body: "final: the fix landed",
        at: "2026-11-06T10:00:00.000Z",
        kind: "deliverable",
      });
      instances[0].emit("thinking", {
        runId: "run-7",
        author: "agent:sam",
        body: "[run run-7] still working",
        at: "2026-11-06T10:00:01.000Z",
        kind: "thinking",
      });
      // A legacy envelope with no kind decodes as the thinking tier (no key).
      instances[0].emit("thinking", {
        runId: "run-7",
        author: "agent:sam",
        body: "[run run-7] legacy row",
        at: "2026-11-06T10:00:02.000Z",
      });
    });
    expect(result.current.map((r) => r.kind ?? "thinking")).toEqual([
      "deliverable",
      "thinking",
      "thinking",
    ]);
  });

  it("fires onEnded exactly once on the run's terminal `ended` milestone", () => {
    const instances = stubEventSource();
    const onEnded = vi.fn();
    renderHook(() =>
      useTicketRunStream("run-7", "in_progress", undefined, onEnded),
    );
    act(() => {
      instances[0].emit("ended", { run_id: "run-7", agent: "sam" });
    });
    expect(onEnded).toHaveBeenCalledTimes(1);
    // a replayed `ended` (SSE reconnect) on the SAME stream must not re-fire.
    act(() => {
      instances[0].emit("ended", { run_id: "run-7", agent: "sam" });
    });
    expect(onEnded).toHaveBeenCalledTimes(1);
  });
});

// ISI-7274 (parent ISI-7262 WS-2): the durable `ticket_comment` echo must
// materialize a committed final comment LIVE. The settle's terminal rows commit
// AFTER the run's `ended` milestone, and a task-io final comment can commit at
// any moment — the ended-keyed re-fetch alone races them. Every new comment
// frame re-fetches the thread (same onEnded callback), so the re-fetch always
// follows the durable row and no manual refresh is needed.
describe("useTicketRunStream — live final comments (ISI-7274)", () => {
  it("fires the re-fetch on each NEW `ticket_comment` frame", () => {
    const instances = stubEventSource();
    const onEnded = vi.fn();
    renderHook(() =>
      useTicketRunStream("run-7", "in_progress", undefined, onEnded),
    );
    act(() => {
      instances[0].emit(
        "ticket_comment",
        {
          workItemId: "wi-1",
          runId: "run-7",
          author: "agent:sam",
          body: "What I did: shipped the seam",
          kind: "summary",
          at: "2026-11-12T10:00:00.000Z",
        },
        "evt-101",
      );
    });
    expect(onEnded).toHaveBeenCalledTimes(1);
    act(() => {
      instances[0].emit(
        "ticket_comment",
        {
          workItemId: "wi-1",
          runId: "run-7",
          author: "ksquad-operator",
          body: "Run succeeded: recorded on the ticket.",
          kind: "summary",
          at: "2026-11-12T10:00:02.000Z",
        },
        "evt-102",
      );
    });
    expect(onEnded).toHaveBeenCalledTimes(2);
    // A replayed comment (SSE reconnect, same outbox id) must NOT re-fire.
    act(() => {
      instances[0].emit(
        "ticket_comment",
        {
          workItemId: "wi-1",
          runId: "run-7",
          author: "ksquad-operator",
          body: "Run succeeded: recorded on the ticket.",
          kind: "summary",
          at: "2026-11-12T10:00:02.000Z",
        },
        "evt-102",
      );
    });
    expect(onEnded).toHaveBeenCalledTimes(2);
  });

  it("fires the re-fetch even AFTER the `ended` milestone already fired (the fast-commit race)", () => {
    const instances = stubEventSource();
    const onEnded = vi.fn();
    renderHook(() =>
      useTicketRunStream("run-7", "in_progress", undefined, onEnded),
    );
    act(() => {
      instances[0].emit("ended", { run_id: "run-7", agent: "sam" }, "evt-201");
    });
    expect(onEnded).toHaveBeenCalledTimes(1);
    // The settle comment commits after `ended` — its echo arrives a frame later
    // and must re-fetch again (this is the exact race the issue reported).
    act(() => {
      instances[0].emit(
        "ticket_comment",
        {
          workItemId: "wi-1",
          runId: "run-7",
          author: "ksquad-operator",
          body: "Run succeeded — agent sam: recorded on the ticket.",
          kind: "summary",
          at: "2026-11-12T10:00:05.000Z",
        },
        "evt-202",
      );
    });
    expect(onEnded).toHaveBeenCalledTimes(2);
  });

  it("keeps `ticket_comment` rows OUT of the live thinking rows (thinking tier is unchanged)", () => {
    const instances = stubEventSource();
    const { result } = renderHook(() =>
      useTicketRunStream("run-7", "in_progress", undefined, () => {}),
    );
    act(() => {
      instances[0].emit("ticket_comment", {
        workItemId: "wi-1",
        runId: "run-7",
        author: "agent:sam",
        body: "final recap",
        kind: "summary",
        at: "2026-11-12T10:00:00.000Z",
      });
    });
    expect(result.current).toEqual([]);
  });
});
