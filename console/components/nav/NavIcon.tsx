// components/nav/NavIcon.tsx — the nav rail icon set (story 8.13, locked UX icon vocabulary).

export type NavIconId =
  | "dashboard"
  | "overview"
  | "compose"
  | "teams"
  | "projects"
  | "agents"
  | "project"
  | "build"
  | "tickets"
  | "runs"
  | "discussion"
  | "settings"
  | "configuration"
  | "otel"
  | "credentials"
  | "plugins"
  | "users"
  | "menu"
  | "close"
  | "lock";

const P: Record<NavIconId, string> = {
  dashboard: "M3 12h7V3H3v9zm0 9h7v-7H3v7zm11 0h7V10h-7v11zm0-18v6h7V3h-7z",
  overview: "M4 4h7v7H4V4zm9 0h7v7h-7V4zM4 13h7v7H4v-7zm9 0h7v7h-7v-7z",
  compose:
    "M12 20h9M16.5 3.5a2.12 2.12 0 013 3L7 19l-4 1 1-4 12.5-12.5z",
  teams:
    "M9 11a3 3 0 100-6 3 3 0 000 6zM3 20v-1c0-2.2 2.7-4 6-4s6 1.8 6 4v1H3zm13-9a3 3 0 10-1-5.8M21 20v-1c0-1.8-1.6-3-3.5-3.6",
  projects:
    "M3 6a2 2 0 012-2h4l2 2h8a2 2 0 012 2v10a2 2 0 01-2 2H5a2 2 0 01-2-2V6z",
  agents:
    "M12 5a2.5 2.5 0 110 5 2.5 2.5 0 010-5zM5 13a2 2 0 110 4 2 2 0 010-4zm14 0a2 2 0 110 4 2 2 0 010-4zM12 12c2.7 0 5 1.2 5 2.8V17H7v-2.2C7 13.2 9.3 12 12 12z",
  project:
    "M3 6a2 2 0 012-2h4l2 2h8a2 2 0 012 2v10a2 2 0 01-2 2H5a2 2 0 01-2-2V6z",
  build: "M13 2L4 14h6l-1 8 9-12h-6l1-8z",
  tickets:
    "M4 5h16v4a2 2 0 000 4v4H4v-4a2 2 0 000-4V5zM10 7v2m0 3v2m0 3v1",
  runs: "M6 3h12v14l-6 4-6-4V3zm2 6h8M8 13h8",
  discussion:
    "M4 4h16v12H8l-4 4V4zm4 4h8M8 12h5",
  settings:
    "M12 8a4 4 0 100 8 4 4 0 000-8zm8.4 4l1.6-1.2-1.6-2.7-1.9.6a7 7 0 00-1.7-1L16.4 4h-3.2l-.6 1.9a7 7 0 00-1.7 1l-1.9-.6L7.4 8.8 9 10a7 7 0 000 2l-1.6 1.2 1.6 2.7 1.9-.6a7 7 0 001.7 1l.6 1.9h3.2l.6-1.9a7 7 0 001.7-1l1.9.6 1.6-2.7L20.4 12a7 7 0 000-.1z",
  configuration:
    "M4 6h16M4 12h16M4 18h16m1-14a1 1 0 100 2 1 1 0 000-2zm0 6a1 1 0 100 2 1 1 0 000-2zm0 6a1 1 0 100 2 1 1 0 000-2z",
  otel: "M3 12h4l2.5 6 4-14L16 15l1.5-3H21",
  credentials:
    "M12 2a5 5 0 015 5v3h1a2 2 0 012 2v8a2 2 0 01-2 2H6a2 2 0 01-2-2v-8a2 2 0 012-2h1V7a5 5 0 015-5zm0 2a3 3 0 00-3 3v3h6V7a3 3 0 00-3-3z",
  plugins: "M9 3v4M15 3v4M7 7h10v4a5 5 0 01-10 0V7zM12 16v5",
  users:
    "M9 11a3 3 0 100-6 3 3 0 000 6zm7 0a3 3 0 100-6 3 3 0 000 6zM3 20v-1c0-2.2 2.7-4 6-4s6 1.8 6 4v1H3zm14 0v-1c0-1.3-.6-2.4-1.5-3.2 2.1.3 4.5 1.4 4.5 3.2v1h-3z",
  menu: "M4 6h16M4 12h16M4 18h16",
  // Padlock (E1-S3 nav lock): rounded shackle + body, stroke style matching the set.
  lock: "M8 11V7a4 4 0 118 0v4M5 11h14v10H5V11z",
  close: "M6 6l12 12M18 6L6 18",
};

export function NavIcon({ id, size = 18 }: { id: string; size?: number }) {
  const d = P[(id as NavIconId) in P ? (id as NavIconId) : "project"];
  return (
    <svg
      width={size}
      height={size}
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth="1.7"
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden="true"
    >
      <path d={d} />
    </svg>
  );
}
