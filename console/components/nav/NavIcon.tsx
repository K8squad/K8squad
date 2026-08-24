// components/nav/NavIcon.tsx — the nav rail icon set (story 8.13, locked UX icon vocabulary).

export type NavIconId =
  | "dashboard"
  | "overview"
  | "agents"
  | "project"
  | "build"
  | "tickets"
  | "runs"
  | "discussion"
  | "settings"
  | "configuration"
  | "credentials"
  | "menu"
  | "close";

const P: Record<NavIconId, string> = {
  dashboard: "M3 12h7V3H3v9zm0 9h7v-7H3v7zm11 0h7V10h-7v11zm0-18v6h7V3h-7z",
  overview: "M4 4h7v7H4V4zm9 0h7v7h-7V4zM4 13h7v7H4v-7zm9 0h7v7h-7v-7z",
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
  credentials:
    "M12 2a5 5 0 015 5v3h1a2 2 0 012 2v8a2 2 0 01-2 2H6a2 2 0 01-2-2v-8a2 2 0 012-2h1V7a5 5 0 015-5zm0 2a3 3 0 00-3 3v3h6V7a3 3 0 00-3-3z",
  menu: "M4 6h16M4 12h16M4 18h16",
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
