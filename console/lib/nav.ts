// lib/nav.ts — the console navigation model (stories 8.13 project-rooted nav + 8.20 adaptive nav).
//
// ONE declarative nav tree drives every surface the shell renders: the desktop/tablet rail, the
// mobile bottom nav + drawer, the project sub-nav tab strip, and the breadcrumb trail. Everything
// is a pure derivation of this tree + the current pathname + the viewer's access level — no client
// store, the URL is the state. RBAC (8.16 seam) is enforced HERE by `visibleNav`: nodes a role may
// not see are removed from the tree BEFORE any breakpoint re-expresses it, so "hidden" always means
// absent from the DOM, never display:none-as-authz. Role arrives with Epic 15.4; until then callers
// pass the default "user".

/** Coarse access level for the RBAC seam (8.16). Ordered least→most privileged. */
export type AccessLevel = "user" | "admin";

const ACCESS_RANK: Record<AccessLevel, number> = { user: 0, admin: 1 };

/** Placeholder a project-scoped href carries until a concrete project id is bound. */
const PROJECT_TOKEN = ":projectId";

/**
 * One node in the nav tree. `id` is the stable UX key (also the NavIcon vocabulary and the
 * active-match token derived from the pathname). A `scope: "project"` node's `href` is a template
 * carrying `:projectId` — bind it with {@link withProject} once a project is selected.
 */
export type NavNode = {
  id: string;
  label: string;
  href: string;
  scope?: "global" | "project";
  /** Minimum access to see this node; defaults to "user" (visible to everyone). */
  requiredAccess?: AccessLevel;
  /**
   * A non-navigating GROUP HEADER (ISI-3725 / ISI-3641 mock `06-onboarding-hub`). A `section` node
   * is NOT a link and NOT a collapsible accordion — the rail renders its `label` as a small-caps
   * muted heading and its `children` as normal, always-visible rail links beneath it. Distinct from a
   * `children` accordion (which stays for project sub-nav). A section node carries `href: ""`; it is
   * never passed to a link renderer. RBAC on the CHILDREN still applies via {@link visibleNav}.
   */
  section?: boolean;
  children?: NavNode[];
};

/** The project sub-nav sections, in UX order (Build · Tickets · Runs · Discussion). */
const PROJECT_SECTIONS: ReadonlyArray<{ id: string; label: string }> = [
  { id: "build", label: "Build" },
  { id: "tickets", label: "Tickets" },
  { id: "runs", label: "Runs" },
  { id: "discussion", label: "Discussion" },
];

/**
 * The canonical top-level nav tree, aligned to the ISI-3641 onboarding redesign mock
 * `06-onboarding-hub` (ISI-3716 deviation spec → ISI-3725). Rail, top→bottom:
 *
 *   Overview · Compose · Teams · Projects · Agents · Runs
 *   ─ SETTINGS ─ (section header)  OTel · Credentials · Plugins · Users & Roles
 *
 * Deltas from the old tree: Dashboard(/`) dropped (Overview is the root); Compose promoted to 2nd;
 * Teams + Runs promoted to top-level items; the old project-scoped `Project` node (Build/Tickets/
 * Runs/Discussion accordion) flattened to a top-level `Projects` link — the project sub-nav tab bar
 * is owned by ISI-3651 E5 (`projectSubnav`, still exported below, unchanged); and Settings becomes a
 * SECTION HEADER grouping OTel/Credentials/Plugins/Users&Roles instead of a single
 * Settings→Configuration accordion. "OTel" retargets the OTLP config surface (`/settings/configuration`,
 * story 8.12 / OtlpConfigScreen — the same screen ISI-3717 Track 2 enriches).
 */
export function navTree(): NavNode[] {
  return [
    { id: "overview", label: "Overview", href: "/overview", scope: "global" },
    {
      // Compose (story 8.5 / UX screen 04-compose-crd): the CRD authoring surface for
      // Team/Project/Agent/Role/Skill. Like Settings → OTel it is a write surface with NO
      // requiredAccess — the apiserver's write-tier membership gate (§13 choke point) is the real
      // wall (viewer → 403, Team creation admin-only), so hiding it in nav would only mislead a
      // contributor who legitimately composes.
      id: "compose",
      label: "Compose",
      href: "/compose",
      scope: "global",
    },
    { id: "teams", label: "Teams", href: "/teams", scope: "global" },
    { id: "projects", label: "Projects", href: "/projects", scope: "global" },
    { id: "agents", label: "Agents", href: "/agents", scope: "global" },
    { id: "runs", label: "Runs", href: "/runs", scope: "global" },
    {
      // SETTINGS — a non-navigating section header (see NavNode.section), NOT the old
      // Settings→Configuration accordion. Its children render as normal rail links beneath a
      // small-caps heading, matching the ISI-3641 mock's grouped settings block.
      id: "settings",
      label: "Settings",
      href: "",
      section: true,
      children: [
        { id: "otel", label: "OTel", href: "/settings/configuration", scope: "global" },
        { id: "credentials", label: "Credentials", href: "/credentials", scope: "global" },
        { id: "plugins", label: "Plugins", href: "/plugins", scope: "global" },
        {
          // Users & Roles (stories 8.15 + 8.16): the admin identity surface. requiredAccess "admin"
          // means visibleNav removes it from the tree for a non-admin BEFORE any breakpoint expresses
          // it — hidden = absent from the DOM (never display:none-as-authz), and the Go apiserver's
          // requireAdmin gate stays the real wall (§13 choke point). Moving it under SETTINGS does not
          // change the seam: visibleNav prunes recursively.
          id: "users",
          label: "Users & Roles",
          href: "/users",
          scope: "global",
          requiredAccess: "admin",
        },
      ],
    },
  ];
}

/** Bind a project-scoped href template (`/projects/:projectId/...`) to a concrete project id. */
export function withProject(path: string, projectId: string): string {
  return path.split(PROJECT_TOKEN).join(encodeURIComponent(projectId));
}

function canAccess(node: NavNode, access: AccessLevel): boolean {
  return ACCESS_RANK[access] >= ACCESS_RANK[node.requiredAccess ?? "user"];
}

/** Prune the tree to the nodes an `access` level may see (recursively). RBAC seam, 8.16. */
export function visibleNav(tree: NavNode[], access: AccessLevel): NavNode[] {
  const out: NavNode[] = [];
  for (const node of tree) {
    if (!canAccess(node, access)) continue;
    out.push(
      node.children
        ? { ...node, children: visibleNav(node.children, access) }
        : node,
    );
  }
  return out;
}

/**
 * The mobile nav split (story 8.20): a role-filtered 5-item bottom nav budget, with everything
 * (top-level + children) flattened into the slide-in drawer for the overflow / sub-nav.
 */
export function mobileNav(
  access: AccessLevel,
  tree: NavNode[] = navTree(),
): { bottom: NavNode[]; drawer: NavNode[] } {
  const visible = visibleNav(tree, access);
  // Section headers (SETTINGS) don't navigate, so they never spend a bottom-nav slot — the budget is
  // the first 5 real destinations. The section's children still reach the drawer via the flatten below.
  const bottom = visible.filter((n) => !n.section).slice(0, 5);
  const drawer: NavNode[] = [];
  for (const node of visible) {
    drawer.push(node.children ? { ...node, children: undefined } : node);
    for (const child of node.children ?? []) drawer.push(child);
  }
  return { bottom, drawer };
}

/** The project-scoped sub-nav tab strip (story 8.13), bound to a concrete project id. */
export function projectSubnav(
  projectId: string,
): Array<{ id: string; label: string; href: string }> {
  return PROJECT_SECTIONS.map((s) => ({
    id: s.id,
    label: s.label,
    href: `/projects/${encodeURIComponent(projectId)}/${s.id}`,
  }));
}

export type Crumb = { label: string; href: string | null };

const SECTION_LABEL: Record<string, string> = {
  overview: "Overview",
  agents: "Agents",
  compose: "Compose",
  teams: "Teams",
  projects: "Projects",
  runs: "Runs",
  plugins: "Plugins",
  settings: "Settings",
  configuration: "Configuration",
  build: "Build",
  tickets: "Tickets",
  discussion: "Discussion",
  users: "Users & Roles",
};

function labelFor(segment: string): string {
  return (
    SECTION_LABEL[segment] ?? segment.charAt(0).toUpperCase() + segment.slice(1)
  );
}

/**
 * The `Home › … › current` breadcrumb trail derived from the pathname. The last crumb is the
 * current location and carries a null href (rendered as plain text, no self-link). Project routes
 * read as `Projects › {projectId} › {section}`.
 */
export function breadcrumbFor(pathname: string): Crumb[] {
  const clean = pathname.split("?")[0].split("#")[0];
  const segments = clean.split("/").filter(Boolean);

  if (segments.length === 0) {
    return [{ label: "Dashboard", href: null }];
  }

  const crumbs: Crumb[] = [{ label: "Dashboard", href: "/" }];

  if (segments[0] === "projects") {
    crumbs.push({ label: "Projects", href: null });
    const projectId = segments[1] ? decodeURIComponent(segments[1]) : null;
    if (projectId) {
      crumbs.push({
        label: projectId,
        href: `/projects/${segments[1]}`,
      });
    }
    if (segments[2]) {
      crumbs.push({ label: labelFor(segments[2]), href: null });
    }
  } else {
    let acc = "";
    for (const segment of segments) {
      acc += `/${segment}`;
      crumbs.push({ label: labelFor(segment), href: acc });
    }
  }

  // Last crumb is the current location — drop its link.
  const last = crumbs[crumbs.length - 1];
  crumbs[crumbs.length - 1] = { label: last.label, href: null };
  return crumbs;
}
