/*
Copyright 2026 The K8squad Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// ksquad-scm-webhook is the story-11.1 webhook ingress: the HMAC-verified
// fast path in front of the repo-sync reconciler (arch §5.4, Epic 9
// Gateway/HTTPRoute routes here).
//
// The load-bearing order (story 11.1 AC4, D8/NFR-SEC8): the per-Project
// HMAC signature is verified BEFORE any byte of the payload is parsed or
// acted on. A bad or absent signature is dropped — 401, no parse, no mirror
// write, no trigger. There is no unsigned diagnostic path.
//
// On a GOOD signature the delivery still never writes mirror state (AC2):
// the handler patches the ksquad.io/scm-sync-trigger annotation on the
// Project, the operator's Project watch fires, and the reconciler runs the
// SAME level-triggered provider snapshot it would have run on the poll
// tick. A redelivery re-bumps the annotation and the extra reconcile is an
// idempotent no-op.
//
// Project identification is explicit — X-KSquad-Project/X-KSquad-Namespace
// headers or ?project=&namespace= query parameters — because the webhook
// secret is per-Project: the signature cannot be checked until the Project
// (and therefore the secret) is known, so the body is NOT parsed for
// identification. Reading the body for HMAC and reading it for meaning are
// different operations; only the first happens before the verify gate.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	ksquadv1alpha1 "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/pkg/controller/reposync"
	"github.com/K8squad/K8squad/pkg/scm"
)

const (
	// webhookSecretKey is the key inside the per-Project webhook Secret
	// holding the HMAC key (spec.repo.sync.webhookSecretRef).
	webhookSecretKey = "webhookSecret"

	// defaultMaxPayloadBytes bounds a delivery body (25 MB — GitHub's own
	// cap). Overridable with -max-payload-bytes for a tighter posture.
	defaultMaxPayloadBytes = 25 << 20

	// maxInFlightDeliveries bounds concurrent deliveries: each holds a
	// bounded body buffer and two API reads, so an unauthenticated flood
	// must not be able to open unbounded concurrency against this pod.
	maxInFlightDeliveries = 64
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(ksquadv1alpha1.AddToScheme(scheme))
}

func main() {
	var listenAddr string
	var maxPayloadBytes int64
	flag.StringVar(&listenAddr, "listen-address", ":8080", "Address the SCM webhook ingress listens on (plaintext HTTP; TLS terminates at the gateway).")
	flag.Int64Var(&maxPayloadBytes, "max-payload-bytes", defaultMaxPayloadBytes, "Maximum accepted webhook delivery body size in bytes.")
	opts := zap.Options{Development: false}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	logger := ctrl.Log.WithName("scm-webhook")

	k8sClient, err := client.New(ctrl.GetConfigOrDie(), client.Options{Scheme: scheme})
	if err != nil {
		logger.Error(err, "unable to create Kubernetes client")
		os.Exit(1)
	}

	h := &webhookHandler{
		client:          k8sClient,
		logger:          logger,
		maxPayloadBytes: maxPayloadBytes,
		inflight:        make(chan struct{}, maxInFlightDeliveries),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("/scm/webhook", h.handle)

	srv := &http.Server{
		Addr:              listenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      60 * time.Second,
	}
	logger.Info("starting ksquad-scm-webhook", "address", listenAddr)
	if err := srv.ListenAndServe(); err != nil {
		if errors.Is(err, http.ErrServerClosed) {
			logger.Info("scm webhook server shut down")
			return
		}
		logger.Error(err, "scm webhook server exited")
		os.Exit(1)
	}
}

// webhookHandler is the HTTP face of the ingress.
type webhookHandler struct {
	client          client.Client
	logger          logr.Logger
	maxPayloadBytes int64
	inflight        chan struct{}
}

// unauthorized is the uniform refusal for everything an unauthenticated
// caller could use to enumerate Projects: unknown project, project without
// repo-sync configured, unresolvable secret, malformed signature, and bad
// signature. The detail lives in the server-side log line only — the HTTP
// face never distinguishes them.
func (h *webhookHandler) unauthorized(w http.ResponseWriter, detail string, logKV ...interface{}) {
	h.logger.Info("webhook: "+detail, logKV...)
	http.Error(w, "unauthorized", http.StatusUnauthorized)
}

// handle enforces the AC4 pipeline: identify Project → resolve secret →
// read body → verify HMAC → (only then) parse header → bump trigger.
func (h *webhookHandler) handle(w http.ResponseWriter, r *http.Request) {
	// Bound concurrent deliveries before anything expensive happens.
	if h.inflight != nil {
		select {
		case h.inflight <- struct{}{}:
			defer func() { <-h.inflight }()
		default:
			http.Error(w, "overloaded", http.StatusServiceUnavailable)
			return
		}
	}

	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	projectName := firstNonEmpty(r.Header.Get("X-KSquad-Project"), r.URL.Query().Get("project"))
	namespace := firstNonEmpty(r.Header.Get("X-KSquad-Namespace"), r.URL.Query().Get("namespace"))
	if projectName == "" {
		// The Project must be identified out-of-band: identifying it from
		// the payload would mean parsing before verify (AC4 regression).
		http.Error(w, "missing project identification (X-KSquad-Project header or ?project=)", http.StatusBadRequest)
		return
	}
	// The namespace is fully attacker-controlled input; a silent "default"
	// fallback would let probes target an unintended namespace. Explicit
	// or reject.
	if namespace == "" {
		http.Error(w, "missing project namespace (X-KSquad-Namespace header or ?namespace=)", http.StatusBadRequest)
		return
	}

	// Unknown project / unconfigured project / unresolvable secret / bad
	// signature are indistinguishable to the caller (uniform 401): the
	// alternatives formed a Project-enumeration oracle.
	project := &ksquadv1alpha1.Project{}
	if err := h.client.Get(r.Context(), client.ObjectKey{Namespace: namespace, Name: projectName}, project); err != nil {
		h.unauthorized(w, "project lookup failed", "project", projectName, "namespace", namespace, "error", err.Error())
		return
	}
	sync := project.Spec.Repo.Sync
	if sync == nil || sync.WebhookSecretRef == nil || sync.WebhookSecretRef.Name == "" {
		h.unauthorized(w, "project has no repo-sync webhook secret configured", "project", projectName)
		return
	}

	maxBytes := h.maxPayloadBytes
	if maxBytes <= 0 {
		maxBytes = defaultMaxPayloadBytes
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBytes))
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			http.Error(w, "payload too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "unreadable body", http.StatusBadRequest)
		return
	}

	// ── THE VERIFY GATE (AC4): everything above is routing, not parsing ──
	secretKey := sync.WebhookSecretRef.Key
	if secretKey == "" {
		secretKey = webhookSecretKey
	}
	secret := &corev1.Secret{}
	if err := h.client.Get(r.Context(), client.ObjectKey{Namespace: namespace, Name: sync.WebhookSecretRef.Name}, secret); err != nil {
		h.unauthorized(w, "webhook secret not resolvable", "project", projectName, "error", err.Error())
		return
	}
	digest, err := scm.ParseSignatureHeader(r.Header.Get("X-Hub-Signature-256"))
	if err != nil {
		h.unauthorized(w, "absent/malformed signature dropped", "project", projectName)
		return
	}
	if !scm.VerifyHMAC(body, string(secret.Data[secretKey]), digest) {
		h.unauthorized(w, "bad signature dropped", "project", projectName)
		return
	}

	// ── verified: payload may NOW be parsed (event header for logging) ──
	eventType := r.Header.Get("X-GitHub-Event")
	if eventType == "" {
		eventType = unknownEvent(body)
	}
	h.logger.Info("webhook: good signature, triggering repo-sync reconcile",
		"project", projectName, "namespace", namespace, "event", eventType)

	// AC2: the delivery only TRIGGERS the level-triggered reconcile — the
	// payload is never written to the mirror. The timestamped annotation
	// wakes the operator's Project watch.
	patch := client.MergeFrom(project.DeepCopy())
	if project.Annotations == nil {
		project.Annotations = map[string]string{}
	}
	project.Annotations[reposync.TriggerAnnotation] = time.Now().UTC().Format(time.RFC3339Nano)
	if err := h.client.Patch(r.Context(), project, patch); err != nil {
		h.logger.Error(err, "webhook: trigger patch failed", "project", projectName)
		http.Error(w, "failed to trigger reconcile", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
	fmt.Fprintln(w, "triggered")
}

// unknownEvent inspects the (already verified) payload for a hint about the
// event type, for logging only. Unparseable payloads still trigger a
// reconcile — the reconcile is level-triggered and never trusts the payload
// anyway (AC2).
func unknownEvent(body []byte) string {
	var probe struct {
		Zen     string `json:"zen"`
		Action  string `json:"action"`
		PullReq *struct {
			Title string `json:"title"`
		} `json:"pull_request"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return "unknown"
	}
	switch {
	case probe.Zen != "":
		return "ping"
	case probe.PullReq != nil:
		return "pull_request/" + probe.Action
	default:
		return "unknown"
	}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
