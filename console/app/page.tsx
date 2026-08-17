// app/page.tsx — console landing / squad-overview mount point (story 8.1).
//
// This is the shell's root route. The full Teams→Projects→Run-status read model (story 8.1) and
// the other 8.x feature screens mount into this shell as they land; this scaffold establishes the
// route so those stories have somewhere to build. Server component by default.

export default function HomePage() {
  return (
    <div>
      <h1>KSquad Console</h1>
      <p className="muted">
        Operator console shell — the BFF choke point (arch §13), the one SSE bus (story 8.2), and
        whole-shell theming (story 8.9) are wired. Feature screens (8.1 squad overview, 8.3–8.11,
        10.3 discussion room) mount into this shell.
      </p>

      <div className="card">
        <h2>Shell status</h2>
        <ul>
          <li>
            <strong>BFF SSE proxy</strong> —{' '}
            <code>GET /api/runs/[runId]/stream</code> (unbuffered, hides the Go apiserver)
          </li>
          <li>
            <strong>BFF build-browser proxy</strong> —{' '}
            <code>GET /api/runs/[runId]/build/[tree|diff|file|meta]</code> (404-verbatim
            existence-hiding)
          </li>
          <li>
            <strong>Shared EventSource</strong> — one client (<code>lib/useRunStream</code>), no
            polling
          </li>
          <li>
            <strong>Theme</strong> — dark/light token swap + v2 8-Crest logo
          </li>
        </ul>
      </div>

      <div className="card">
        <h2>Live Run stream</h2>
        <p className="muted">
          Open a run at <code>/runs/&lt;runId&gt;</code> to watch its coordination events stream
          live through the BFF.
        </p>
      </div>
    </div>
  );
}
