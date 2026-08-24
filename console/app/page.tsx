// app/page.tsx — Dashboard (fleet) mount point (story 8.13 nav root; screen 8.8).
//
// Dashboard is GLOBAL (not project-scoped) and tops the rail. The fleet dashboard screen
// itself (8.8: health/consumption/live mapping) is built under ISI-2906; this interim mount
// keeps the nav hierarchy honest — the rail's Dashboard node lands here.

export default function DashboardPage() {
  return (
    <div>
      <h1>Dashboard</h1>
      <p className="muted">
        Fleet-level view — health, consumption, live mapping (story 8.8,
        [ISI-2906](/ISI/issues/ISI-2906) in flight).
      </p>

      <div className="card">
        <h2>Shell status</h2>
        <ul>
          <li>
            <strong>BFF SSE proxy</strong> —{" "}
            <code>GET /api/runs/[runId]/stream</code> (unbuffered, hides the Go
            apiserver)
          </li>
          <li>
            <strong>BFF build-browser proxy</strong> —{" "}
            <code>GET /api/runs/[runId]/build/[tree|diff|file|meta]</code>{" "}
            (404-verbatim existence-hiding)
          </li>
          <li>
            <strong>Shared EventSource</strong> — one client (
            <code>lib/useRunStream</code>), no polling
          </li>
          <li>
            <strong>Theme</strong> — dark/light token swap + v2 8-Crest logo
          </li>
          <li>
            <strong>Nav</strong> — Project-rooted adaptive shell (8.13/8.20)
          </li>
        </ul>
      </div>

      <div className="card">
        <h2>Live Run stream</h2>
        <p className="muted">
          Open a run at <code>/runs/&lt;runId&gt;</code> to watch its
          coordination events stream live through the BFF.
        </p>
      </div>
    </div>
  );
}
