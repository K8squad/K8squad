"use client";

// components/nav/NavErrorBoundary.tsx — a scoped error boundary for a dynamic rail sub-tree
// (ISI-4001). The Teams island (TeamsNavTree) fetches at runtime; if it throws, the rest of the
// rail (Overview, Projects, Settings, …) must keep working. This boundary catches a render
// exception in its subtree and swaps in a static fallback (the plain Teams link), so a sub-tree
// failure degrades to a leaf link instead of blanking the whole shell.

import { Component, type ReactNode } from "react";

export class NavErrorBoundary extends Component<
  { children: ReactNode; fallback: ReactNode },
  { failed: boolean }
> {
  constructor(props: { children: ReactNode; fallback: ReactNode }) {
    super(props);
    this.state = { failed: false };
  }

  static getDerivedStateFromError() {
    return { failed: true };
  }

  render() {
    return this.state.failed ? this.props.fallback : this.props.children;
  }
}
