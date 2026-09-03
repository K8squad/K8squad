// app/plugins/page.tsx — Settings › Plugins nav destination (ISI-3725 rail realignment to ISI-3641).
//
// The ISI-3641 rail's SETTINGS group introduces a Plugins item. No plugins management surface exists
// yet; this is the honest landing so the rail link resolves instead of 404-ing. ponytail: placeholder
// route, no data fetch — upgrade to the plugins management surface when that story lands.

export const metadata = {
  title: "Plugins — K8squad Console",
};

export default function PluginsPage() {
  return (
    <section className="stub">
      <h1>Plugins</h1>
      <p>Plugin management is coming. This surface will list and configure installed plugins.</p>
    </section>
  );
}
