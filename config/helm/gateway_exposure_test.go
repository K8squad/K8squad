// Guard tests for the optional Gateway API exposure surface (ISI-3515). Renders
// the chart with `helm template` and asserts the behaviours the ticket + the
// Copilot review require:
//   - default-off: no Gateway/HTTPRoute leaks into a normal install;
//   - fail-fast on each invalid enabled config (no gatewayClassName, no
//     controlPlane, identical hostnames);
//   - HTTP-only IP mode renders a Gateway + ONLY the console HTTPRoute (the
//     console owns `/` incl. the `/api/*` BFF); the apiserver gets NO browser
//     route on a shared IP — a `/api` apiserver route would shadow the BFF and
//     break login (ISI-3530). The direct apiserver route appears only once a
//     dedicated `hostnames.apiserver` is set;
//   - HTTPS + redirect renders an https listener and a redirect that targets the
//     CONFIGURED https port (not a derived 443).
//
// Skips (never fails) when the `helm` binary is absent, matching this repo's
// skip-with-reason convention for tool-gated lanes.
package helm

import (
	"os/exec"
	"strings"
	"testing"
)

// renderGW runs `helm template` with the control plane on and NATS off (so the
// bundled-bus storage-class guard never masks the exposure fail-fast we are
// exercising), then appends the caller's exposure flags.
func renderGW(t *testing.T, args ...string) (string, error) {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm binary not on PATH; skipping chart-render guard")
	}
	base := []string{"template", "t", ".",
		"--set", "controlPlane.enabled=true",
		"--set", "controlPlane.database.dsn=postgres://u@h/db",
		"--set", "controlPlane.nats.enabled=false"}
	out, err := exec.Command("helm", append(base, args...)...).CombinedOutput()
	return string(out), err
}

// Default install (exposure off) must not render any Gateway API object.
func TestGatewayDefaultOff(t *testing.T) {
	out, err := renderGW(t)
	if err != nil {
		t.Fatalf("render failed: %v\n%s", err, out)
	}
	for _, unwanted := range []string{"kind: Gateway", "kind: HTTPRoute"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("default-off render leaked %q — exposure must stay off by default", unwanted)
		}
	}
}

// enabled=true without a gatewayClassName must fail fast.
func TestGatewayRequiresClassName(t *testing.T) {
	out, err := renderGW(t, "--set", "exposure.gateway.enabled=true")
	if err == nil {
		t.Fatalf("expected fail-fast without gatewayClassName; render succeeded:\n%s", out)
	}
	if !strings.Contains(out, "gatewayClassName") {
		t.Errorf("fail message should name gatewayClassName; got:\n%s", out)
	}
}

// enabled=true without controlPlane.enabled must fail fast (the routes target
// Services that only exist with the control plane running).
func TestGatewayRequiresControlPlane(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm binary not on PATH; skipping chart-render guard")
	}
	out, err := exec.Command("helm", "template", "t", ".",
		"--set", "exposure.gateway.enabled=true",
		"--set", "exposure.gateway.gatewayClassName=kgateway").CombinedOutput()
	if err == nil {
		t.Fatalf("expected fail-fast without controlPlane.enabled; render succeeded:\n%s", out)
	}
	if !strings.Contains(string(out), "controlPlane.enabled") {
		t.Errorf("fail message should name controlPlane.enabled; got:\n%s", out)
	}
}

// Identical non-empty hostnames must fail fast — the two routes would collide on
// the same host+path and Gateway API would silently drop one backend.
func TestGatewayRejectsIdenticalHostnames(t *testing.T) {
	out, err := renderGW(t,
		"--set", "exposure.gateway.enabled=true",
		"--set", "exposure.gateway.gatewayClassName=kgateway",
		"--set", "exposure.gateway.hostnames.console=ksquad.example.com",
		"--set", "exposure.gateway.hostnames.apiserver=ksquad.example.com")
	if err == nil {
		t.Fatalf("expected fail-fast on identical hostnames; render succeeded:\n%s", out)
	}
	if !strings.Contains(out, "identical") {
		t.Errorf("fail message should flag identical hostnames; got:\n%s", out)
	}
}

// HTTP-only IP mode: Gateway + ONLY the console HTTPRoute (console owns `/`
// including the `/api/*` BFF); no apiserver route, no hostnames (ISI-3530).
func TestGatewayHTTPOnlyIPMode(t *testing.T) {
	out, err := renderGW(t,
		"--set", "exposure.gateway.enabled=true",
		"--set", "exposure.gateway.gatewayClassName=kgateway")
	if err != nil {
		t.Fatalf("render failed: %v\n%s", err, out)
	}
	for _, want := range []string{
		"kind: Gateway",
		`gatewayClassName: "kgateway"`,
		"name: ksquad-console", // the sole HTTPRoute in IP mode
	} {
		if !strings.Contains(out, want) {
			t.Errorf("HTTP-only IP render missing %q:\n%s", want, out)
		}
	}
	// Exactly one HTTPRoute (console) — a `/api` apiserver route in IP mode would
	// shadow the console BFF and break login (ISI-3530).
	if n := strings.Count(out, "kind: HTTPRoute"); n != 1 {
		t.Errorf("IP mode must render exactly one HTTPRoute (console); got %d:\n%s", n, out)
	}
	if strings.Contains(out, `value: "/api"`) {
		t.Errorf("IP mode must NOT route the apiserver under /api (shadows the BFF):\n%s", out)
	}
	if strings.Contains(out, "hostnames:") {
		t.Errorf("IP mode must not render any hostnames block:\n%s", out)
	}
}

// With a dedicated apiserver hostname the direct apiserver HTTPRoute DOES render
// (host-specificity keeps it collision-free with the console BFF) — the escape
// hatch that ISI-3530 preserved for direct apiserver ingress.
func TestGatewayApiserverRouteWithHostname(t *testing.T) {
	out, err := renderGW(t,
		"--set", "exposure.gateway.enabled=true",
		"--set", "exposure.gateway.gatewayClassName=kgateway",
		"--set", "exposure.gateway.hostnames.apiserver=api.ksquad.example.com")
	if err != nil {
		t.Fatalf("render failed: %v\n%s", err, out)
	}
	if n := strings.Count(out, "kind: HTTPRoute"); n != 2 {
		t.Errorf("apiserver-hostname mode should render two HTTPRoutes (console + apiserver); got %d:\n%s", n, out)
	}
	for _, want := range []string{
		"name: ksquad-apiserver",
		"api.ksquad.example.com",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("apiserver-hostname render missing %q:\n%s", want, out)
		}
	}
}

// HTTPS + redirect on a non-default port: the redirect route must target the
// configured https listener port, not a derived 443.
func TestGatewayHTTPSRedirectPort(t *testing.T) {
	out, err := renderGW(t,
		"--set", "exposure.gateway.enabled=true",
		"--set", "exposure.gateway.gatewayClassName=kgateway",
		"--set", "exposure.gateway.listeners.https.enabled=true",
		"--set", "exposure.gateway.listeners.https.certSecretName=ksquad-tls",
		"--set", "exposure.gateway.listeners.https.port=8443",
		"--set", "exposure.gateway.httpsRedirect=true")
	if err != nil {
		t.Fatalf("render failed: %v\n%s", err, out)
	}
	for _, want := range []string{
		"protocol: HTTPS",
		"name: ksquad-https-redirect",
		"port: 8443", // redirect (and listener) pinned to the configured port
	} {
		if !strings.Contains(out, want) {
			t.Errorf("HTTPS+redirect render missing %q:\n%s", want, out)
		}
	}
}
