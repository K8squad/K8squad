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

package instscore

import (
	"strings"
	"testing"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

// ruleCase is one (rule, scenario) expectation.
type ruleCase struct {
	name   string
	fold   func(*Accumulator)
	status Status
	// evidenceContains, when set, must appear in the verdict evidence.
	evidenceContains string
}

func runRuleCases(t *testing.T, ruleID string, cases []ruleCase) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := evaluateRule(t, ruleID, tc.fold)
			if v.Status != tc.status {
				t.Fatalf("%s/%s: status %q, want %q (evidence: %s)", ruleID, tc.name, v.Status, tc.status, v.Evidence)
			}
			if tc.evidenceContains != "" && !strings.Contains(v.Evidence, tc.evidenceContains) {
				t.Fatalf("%s/%s: evidence %q missing %q", ruleID, tc.name, v.Evidence, tc.evidenceContains)
			}
		})
	}
}

func TestEvalLOG001(t *testing.T) {
	day := uint64(86400)
	runRuleCases(t, "LOG-001", []ruleCase{
		{
			name: "debug in production beyond 14 days fails",
			fold: func(a *Accumulator) {
				r := goodResource("svc", "i", "p", "u")
				a.AcceptLogs(logReq2(logsReq(r,
					logRecord(ns(0), "DEBUG", 5, "probe"),
					logRecord(ns(15*day), "DEBUG", 5, "probe"))))
			},
			status:           StatusFail,
			evidenceContains: "spanning 15.0 days",
		},
		{
			name: "debug in production within 14 days passes",
			fold: func(a *Accumulator) {
				r := goodResource("svc", "i", "p", "u")
				a.AcceptLogs(logReq2(logsReq(r,
					logRecord(ns(0), "DEBUG", 5, "probe"),
					logRecord(ns(14*day), "DEBUG", 5, "probe"))))
			},
			status: StatusPass,
		},
		{
			name: "debug outside production passes",
			fold: func(a *Accumulator) {
				r := goodResource("svc", "i", "p", "u")
				replaceAttr(r, "deployment.environment.name", "staging")
				a.AcceptLogs(logReq2(logsReq(r, logRecord(ns(20*day), "DEBUG", 5, "probe"))))
			},
			status: StatusPass,
		},
		{
			name: "no logs is no-data fail",
			fold: func(a *Accumulator) {
				a.AcceptTraces(traceReq(spansReq(goodResource("svc", "i", "p", "u"),
					span("t", "s", "", tracepb.Span_SPAN_KIND_SERVER, "x", ns(1), ns(2)))))
			},
			status:           StatusFail,
			evidenceContains: "no Log observed in window",
		},
	})
}

func replaceAttr(r *resourcepb.Resource, key, value string) {
	for i, kvv := range r.Attributes {
		if kvv.GetKey() == key {
			r.Attributes[i] = kv(key, value)
			return
		}
	}
	r.Attributes = append(r.Attributes, kv(key, value))
}

func TestEvalLOG002(t *testing.T) {
	runRuleCases(t, "LOG-002", []ruleCase{
		{
			name: "unset severity number fails",
			fold: func(a *Accumulator) {
				a.AcceptLogs(logReq2(logsReq(goodResource("svc", "i", "p", "u"),
					logRecord(ns(1), "INFO", 9, "ok"),
					logRecord(ns(2), "", 0, "converted from file"))))
			},
			status: StatusFail,
		},
		{
			name: "all records carry severity passes",
			fold: func(a *Accumulator) {
				a.AcceptLogs(logReq2(logsReq(goodResource("svc", "i", "p", "u"),
					logRecord(ns(1), "INFO", 9, "ok"))))
			},
			status: StatusPass,
		},
	})
}

func TestEvalLOG003SensitiveFormats(t *testing.T) {
	cases := []struct {
		name  string
		body  string
		attrs []*commonpb.KeyValue
	}{
		{"email in body", "login failed for alice@example.com from 10.0.0.1", nil},
		{"luhn card in body", "payment processed for card 4111 1111 1111 1111", nil},
		{"ssn in body", `{"user":{"ssn":"123-45-6789"}}`, nil},
		{"bearer token in body", "Authorization: Bearer eyJhbGciOiJIUzI1NiJ9.e30.sig", nil},
		{"api key in body", "using key sk-proj-abc123def456ghi789", nil},
		{"password kv in body", `{"password":"correct-horse"}`, nil},
		{"email in attribute", "request handled", []*commonpb.KeyValue{kv("user.email", "alice@example.com")}},
		{"sensitive key with any value", "request handled", []*commonpb.KeyValue{kv("custom.password", "nothing suspicious")}},
		{"int card in attribute", "request handled", []*commonpb.KeyValue{kvInt("card", 4111111111111111)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := evaluateRule(t, "LOG-003", func(a *Accumulator) {
				a.AcceptLogs(logReq2(logsReq(goodResource("svc", "i", "p", "u"),
					logRecord(ns(1), "INFO", 9, tc.body, tc.attrs...))))
			})
			if v.Status != StatusFail {
				t.Fatalf("status %q, want fail (evidence: %s)", v.Status, v.Evidence)
			}
		})
	}
	// Control: clean record passes; nested array values are scanned.
	t.Run("clean passes", func(t *testing.T) {
		v := evaluateRule(t, "LOG-003", func(a *Accumulator) {
			a.AcceptLogs(logReq2(logsReq(goodResource("svc", "i", "p", "u"),
				logRecord(ns(1), "INFO", 9, "request handled in 42ms"))))
		})
		if v.Status != StatusPass {
			t.Fatalf("status %q, want pass (evidence: %s)", v.Status, v.Evidence)
		}
	})
	t.Run("nested array email fails", func(t *testing.T) {
		v := evaluateRule(t, "LOG-003", func(a *Accumulator) {
			r := goodResource("svc", "i", "p", "u")
			a.AcceptLogs(logReq2(logsReq(r, &logspb.LogRecord{
				SeverityText:   "INFO",
				SeverityNumber: 9,
				Body: &commonpb.AnyValue{Value: &commonpb.AnyValue_ArrayValue{ArrayValue: &commonpb.ArrayValue{
					Values: []*commonpb.AnyValue{{Value: &commonpb.AnyValue_StringValue{StringValue: "context: bob@example.com"}}},
				}}},
			})))
		})
		if v.Status != StatusFail {
			t.Fatalf("status %q, want fail (evidence: %s)", v.Status, v.Evidence)
		}
	})
}

func TestEvalMET001(t *testing.T) {
	runRuleCases(t, "MET-001", []ruleCase{
		{
			name: "cardinality under bound passes",
			fold: func(a *Accumulator) {
				r := goodResource("svc", "i", "p", "u")
				a.AcceptMetrics(metricsReq(metricsReqFor(r,
					gaugeMetric("http.requests", "{req}", ns(1), kv("route", "/a")),
					gaugeMetric("http.requests", "{req}", ns(1), kv("route", "/b")))))
			},
			status: StatusPass,
		},
		{
			name: "cardinality over bound fails",
			fold: func(a *Accumulator) {
				r := goodResource("svc", "i", "p", "u")
				var ms []*metricspb.Metric
				for i := 0; i < 10_000; i++ {
					ms = append(ms, gaugeMetric("high.card", "{obj}", ns(1), kv("user", itoa(i))))
				}
				a.AcceptMetrics(metricsReq(metricsReqFor(r, ms...)))
			},
			status: StatusFail,
		},
		{
			name: "same values in separate hours are separate windows",
			fold: func(a *Accumulator) {
				r := goodResource("svc", "i", "p", "u")
				hour := uint64(3_600)
				var ms []*metricspb.Metric
				for i := 0; i < 6_000; i++ {
					ms = append(ms,
						gaugeMetric("high.card", "{obj}", ns(0), kv("user", itoa(i))),
						gaugeMetric("high.card", "{obj}", ns(hour), kv("user", itoa(i))))
				}
				a.AcceptMetrics(metricsReq(metricsReqFor(r, ms...)))
			},
			status: StatusPass,
		},
	})
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [20]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(b[pos:])
}

func TestEvalMET002(t *testing.T) {
	runRuleCases(t, "MET-002", []ruleCase{
		{
			name: "missing unit fails",
			fold: func(a *Accumulator) {
				a.AcceptMetrics(metricsReq(metricsReqFor(goodResource("svc", "i", "p", "u"),
					gaugeMetric("cpu.usage", "", ns(1)))))
			},
			status: StatusFail,
		},
		{
			name: "word-form unit fails as non-ucum",
			fold: func(a *Accumulator) {
				a.AcceptMetrics(metricsReq(metricsReqFor(goodResource("svc", "i", "p", "u"),
					gaugeMetric("cpu.usage", "milliseconds", ns(1)))))
			},
			status: StatusFail,
		},
		{
			name: "ucum code unit passes",
			fold: func(a *Accumulator) {
				a.AcceptMetrics(metricsReq(metricsReqFor(goodResource("svc", "i", "p", "u"),
					gaugeMetric("cpu.usage", "%", ns(1)))))
			},
			status: StatusPass,
		},
	})
}

func TestEvalMET003And004(t *testing.T) {
	runRuleCases(t, "MET-003", []ruleCase{
		{
			name: "one name two units fails",
			fold: func(a *Accumulator) {
				r := goodResource("svc", "i", "p", "u")
				a.AcceptMetrics(metricsReq(metricsReqFor(r,
					gaugeMetric("request.duration", "ms", ns(1)),
					gaugeMetric("request.duration", "s", ns(2)))))
			},
			status:           StatusFail,
			evidenceContains: "request.duration",
		},
		{
			name: "consistent units pass",
			fold: func(a *Accumulator) {
				r := goodResource("svc", "i", "p", "u")
				a.AcceptMetrics(metricsReq(metricsReqFor(r,
					gaugeMetric("request.duration", "ms", ns(1)),
					gaugeMetric("request.duration", "ms", ns(2)),
					gaugeMetric("queue.depth", "{obj}", ns(1)))))
			},
			status: StatusPass,
		},
	})
	runRuleCases(t, "MET-004", []ruleCase{
		{
			name: "one name two bucket sets fails",
			fold: func(a *Accumulator) {
				r := goodResource("svc", "i", "p", "u")
				a.AcceptMetrics(metricsReq(metricsReqFor(r,
					histMetric("latency", "ms", ns(1), []float64{1, 5, 10}),
					histMetric("latency", "ms", ns(2), []float64{1, 10, 50}))))
			},
			status: StatusFail,
		},
		{
			name: "consistent buckets pass",
			fold: func(a *Accumulator) {
				r := goodResource("svc", "i", "p", "u")
				a.AcceptMetrics(metricsReq(metricsReqFor(r,
					histMetric("latency", "ms", ns(1), []float64{1, 5, 10}),
					histMetric("latency", "ms", ns(2), []float64{1, 5, 10}))))
			},
			status: StatusPass,
		},
		{
			name: "no histograms passes",
			fold: func(a *Accumulator) {
				a.AcceptMetrics(metricsReq(metricsReqFor(goodResource("svc", "i", "p", "u"),
					gaugeMetric("queue.depth", "{obj}", ns(1)))))
			},
			status: StatusPass,
		},
	})
}

func TestEvalMET005(t *testing.T) {
	runRuleCases(t, "MET-005", []ruleCase{
		{
			name: "unit word in name fails (vendored example)",
			fold: func(a *Accumulator) {
				a.AcceptMetrics(metricsReq(metricsReqFor(goodResource("svc", "i", "p", "u"),
					gaugeMetric("collection.duration.seconds", "s", ns(1)))))
			},
			status: StatusFail,
		},
		{
			name: "milli prefix resolved to word fails",
			fold: func(a *Accumulator) {
				a.AcceptMetrics(metricsReq(metricsReqFor(goodResource("svc", "i", "p", "u"),
					gaugeMetric("request.duration.milliseconds", "ms", ns(1)))))
			},
			status: StatusFail,
		},
		{
			name: "clean name passes",
			fold: func(a *Accumulator) {
				a.AcceptMetrics(metricsReq(metricsReqFor(goodResource("svc", "i", "p", "u"),
					gaugeMetric("request.duration", "ms", ns(1)))))
			},
			status: StatusPass,
		},
		{
			name: "single-letter unit never substring-matches",
			fold: func(a *Accumulator) {
				a.AcceptMetrics(metricsReq(metricsReqFor(goodResource("svc", "i", "p", "u"),
					gaugeMetric("systems.uptime", "s", ns(1)))))
			},
			status: StatusPass,
		},
	})
}

func TestEvalMET006(t *testing.T) {
	runRuleCases(t, "MET-006", []ruleCase{
		{
			name: "metric named as semconv key fails (vendored example)",
			fold: func(a *Accumulator) {
				a.AcceptMetrics(metricsReq(metricsReqFor(goodResource("svc", "i", "p", "u"),
					gaugeMetric("http.response.status_code", "{code}", ns(1)))))
			},
			status:           StatusFail,
			evidenceContains: "http.response.status_code",
		},
		{
			name: "ordinary name passes",
			fold: func(a *Accumulator) {
				a.AcceptMetrics(metricsReq(metricsReqFor(goodResource("svc", "i", "p", "u"),
					gaugeMetric("http.requests", "{req}", ns(1)))))
			},
			status: StatusPass,
		},
	})
}

func TestEvalMET007(t *testing.T) {
	runRuleCases(t, "MET-007", []ruleCase{
		{
			name: "volatile resource attribute on metrics fails",
			fold: func(a *Accumulator) {
				r := goodResource("svc", "i", "p", "u")
				r.Attributes = append(r.Attributes, kv("process.command_args", "/bin/app --port=8080"))
				a.AcceptMetrics(metricsReq(metricsReqFor(r, gaugeMetric("queue.depth", "{obj}", ns(1)))))
			},
			status: StatusFail,
		},
		{
			name: "no metrics in window is no-data fail",
			fold: func(a *Accumulator) {
				r := goodResource("svc", "i", "p", "u")
				r.Attributes = append(r.Attributes, kv("process.command_args", "/bin/app --port=8080"))
				a.AcceptTraces(traceReq(spansReq(r,
					span("t", "s", "", tracepb.Span_SPAN_KIND_SERVER, "x", ns(1), ns(6)))))
			},
			status:           StatusFail,
			evidenceContains: "no Metric observed in window",
		},
		{
			name: "clean resource on metrics passes",
			fold: func(a *Accumulator) {
				a.AcceptMetrics(metricsReq(metricsReqFor(goodResource("svc", "i", "p", "u"),
					gaugeMetric("queue.depth", "{obj}", ns(1)))))
			},
			status: StatusPass,
		},
	})
}

func TestEvalMET008(t *testing.T) {
	runRuleCases(t, "MET-008", []ruleCase{
		{
			name: "sensitive datapoint attribute fails",
			fold: func(a *Accumulator) {
				a.AcceptMetrics(metricsReq(metricsReqFor(goodResource("svc", "i", "p", "u"),
					gaugeMetric("login.failures", "{err}", ns(1), kv("user", "alice@example.com")))))
			},
			status: StatusFail,
		},
		{
			name: "sensitive metric name fails",
			fold: func(a *Accumulator) {
				a.AcceptMetrics(metricsReq(metricsReqFor(goodResource("svc", "i", "p", "u"),
					gaugeMetric("orders.alice@example.com.count", "{obj}", ns(1)))))
			},
			status: StatusFail,
		},
		{
			name: "clean metric passes",
			fold: func(a *Accumulator) {
				a.AcceptMetrics(metricsReq(metricsReqFor(goodResource("svc", "i", "p", "u"),
					gaugeMetric("login.failures", "{err}", ns(1), kv("route", "/login")))))
			},
			status: StatusPass,
		},
	})
}

func TestEvalRES001and002(t *testing.T) {
	runRuleCases(t, "RES-001", []ruleCase{
		{
			name: "missing instance id fails",
			fold: func(a *Accumulator) {
				a.AcceptTraces(traceReq(spansReq(res(kv("service.name", "svc")),
					span("t", "s", "", tracepb.Span_SPAN_KIND_SERVER, "x", ns(1), ns(6)))))
			},
			status: StatusFail,
		},
		{
			name: "present passes",
			fold: func(a *Accumulator) {
				a.AcceptTraces(traceReq(spansReq(goodResource("svc", "i-1", "pod-1", "uid-1"),
					span("t", "s", "", tracepb.Span_SPAN_KIND_SERVER, "x", ns(1), ns(6)))))
			},
			status: StatusPass,
		},
	})
	runRuleCases(t, "RES-002", []ruleCase{
		{
			name: "instance id shared across pods fails (vendored example shape)",
			fold: func(a *Accumulator) {
				a.AcceptTraces(traceReq(
					spansReq(goodResource("payments", "abc", "payment-abc123", "uid-a"),
						span("t", "s1", "", tracepb.Span_SPAN_KIND_SERVER, "x", ns(1), ns(6))),
					spansReq(goodResource("payments", "abc", "payment-def456", "uid-b"),
						span("t2", "s2", "", tracepb.Span_SPAN_KIND_SERVER, "y", ns(1), ns(6)))))
			},
			status:           StatusFail,
			evidenceContains: "abc",
		},
		{
			name: "unique instance ids pass",
			fold: func(a *Accumulator) {
				a.AcceptTraces(traceReq(
					spansReq(goodResource("payments", "i-1", "payment-abc123", "uid-a"),
						span("t", "s1", "", tracepb.Span_SPAN_KIND_SERVER, "x", ns(1), ns(6))),
					spansReq(goodResource("payments", "i-2", "payment-def456", "uid-b"),
						span("t2", "s2", "", tracepb.Span_SPAN_KIND_SERVER, "y", ns(1), ns(6)))))
			},
			status: StatusPass,
		},
		{
			name: "same pod re-exporting is one logical resource",
			fold: func(a *Accumulator) {
				r := goodResource("payments", "i-1", "payment-abc123", "uid-a")
				a.AcceptTraces(traceReq(
					spansReq(r, span("t", "s1", "", tracepb.Span_SPAN_KIND_SERVER, "x", ns(1), ns(6))),
					spansReq(r, span("t2", "s2", "", tracepb.Span_SPAN_KIND_SERVER, "y", ns(1), ns(6)))))
			},
			status: StatusPass,
		},
	})
}

func TestEvalRES003(t *testing.T) {
	runRuleCases(t, "RES-003", []ruleCase{
		{
			name: "k8s resource without pod uid fails",
			fold: func(a *Accumulator) {
				r := res(kv("service.name", "svc"), kv("service.instance.id", "i"),
					kv("deployment.environment.name", "production"),
					kv("k8s.namespace.name", "default"), kv("k8s.pod.name", "pod-1"))
				a.AcceptTraces(traceReq(spansReq(r,
					span("t", "s", "", tracepb.Span_SPAN_KIND_SERVER, "x", ns(1), ns(6)))))
			},
			status: StatusFail,
		},
		{
			name: "non-k8s resource passes",
			fold: func(a *Accumulator) {
				a.AcceptTraces(traceReq(spansReq(goodResource("svc", "i", "", ""),
					span("t", "s", "", tracepb.Span_SPAN_KIND_SERVER, "x", ns(1), ns(6)))))
			},
			status: StatusPass,
		},
	})
}

func TestEvalRES004(t *testing.T) {
	runRuleCases(t, "RES-004", []ruleCase{
		{
			name: "resource-level key on span fails (vendored example)",
			fold: func(a *Accumulator) {
				a.AcceptTraces(traceReq(spansReq(goodResource("svc", "i", "p", "u"),
					span("t", "s", "", tracepb.Span_SPAN_KIND_SERVER, "x", ns(1), ns(6), kv("service.name", "svc")))))
			},
			status:           StatusFail,
			evidenceContains: "service.name",
		},
		{
			name: "record-level key on resource fails (vendored example)",
			fold: func(a *Accumulator) {
				r := goodResource("svc", "i", "p", "u")
				r.Attributes = append(r.Attributes, kv("http.request.method", "GET"))
				a.AcceptTraces(traceReq(spansReq(r,
					span("t", "s", "", tracepb.Span_SPAN_KIND_SERVER, "x", ns(1), ns(6)))))
			},
			status:           StatusFail,
			evidenceContains: "http.request.method",
		},
		{
			name: "keys at right level pass",
			fold: func(a *Accumulator) {
				a.AcceptTraces(traceReq(spansReq(goodResource("svc", "i", "p", "u"),
					span("t", "s", "", tracepb.Span_SPAN_KIND_SERVER, "x", ns(1), ns(6), kv("http.request.method", "GET")))))
			},
			status: StatusPass,
		},
	})
}

func TestEvalRES005Through007(t *testing.T) {
	runRuleCases(t, "RES-005", []ruleCase{
		{
			name: "empty service name fails",
			fold: func(a *Accumulator) {
				a.AcceptTraces(traceReq(spansReq(res(kv("service.version", "1.0")),
					span("t", "s", "", tracepb.Span_SPAN_KIND_SERVER, "x", ns(1), ns(6)))))
			},
			status: StatusFail,
		},
	})
	runRuleCases(t, "RES-006", []ruleCase{
		{
			name: "uppercase well-known value fails",
			fold: func(a *Accumulator) {
				r := goodResource("svc", "i", "p", "u")
				replaceAttr(r, "service.criticality", "HIGH")
				a.AcceptTraces(traceReq(spansReq(r,
					span("t", "s", "", tracepb.Span_SPAN_KIND_SERVER, "x", ns(1), ns(6)))))
			},
			status: StatusFail,
		},
		{
			name: "custom value passes",
			fold: func(a *Accumulator) {
				r := goodResource("svc", "i", "p", "u")
				replaceAttr(r, "service.criticality", "mission-ambiguous")
				a.AcceptTraces(traceReq(spansReq(r,
					span("t", "s", "", tracepb.Span_SPAN_KIND_SERVER, "x", ns(1), ns(6)))))
			},
			status: StatusPass,
		},
		{
			name: "absent passes",
			fold: func(a *Accumulator) {
				r := goodResource("svc", "i", "p", "u")
				filterOut(r, "service.criticality")
				a.AcceptTraces(traceReq(spansReq(r,
					span("t", "s", "", tracepb.Span_SPAN_KIND_SERVER, "x", ns(1), ns(6)))))
			},
			status: StatusPass,
		},
	})
	runRuleCases(t, "RES-007", []ruleCase{
		{
			name: "missing environment fails",
			fold: func(a *Accumulator) {
				r := goodResource("svc", "i", "p", "u")
				filterOut(r, "deployment.environment.name")
				a.AcceptTraces(traceReq(spansReq(r,
					span("t", "s", "", tracepb.Span_SPAN_KIND_SERVER, "x", ns(1), ns(6)))))
			},
			status: StatusFail,
		},
	})
}

func filterOut(r *resourcepb.Resource, key string) {
	out := r.Attributes[:0]
	for _, kvv := range r.Attributes {
		if kvv.GetKey() != key {
			out = append(out, kvv)
		}
	}
	r.Attributes = out
}

func TestEvalRES010(t *testing.T) {
	runRuleCases(t, "RES-010", []ruleCase{
		{
			name: "credential in command args fails (vendored example)",
			fold: func(a *Accumulator) {
				r := goodResource("svc", "i", "p", "u")
				r.Attributes = append(r.Attributes, &commonpb.KeyValue{
					Key: "process.command_args",
					Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_ArrayValue{ArrayValue: &commonpb.ArrayValue{
						Values: []*commonpb.AnyValue{
							{Value: &commonpb.AnyValue_StringValue{StringValue: "java"}},
							{Value: &commonpb.AnyValue_StringValue{StringValue: "-Djavax.net.ssl.trustStorePassword=MyS3cret!"}},
						},
					}}},
				})
				a.AcceptTraces(traceReq(spansReq(r,
					span("t", "s", "", tracepb.Span_SPAN_KIND_SERVER, "x", ns(1), ns(6)))))
			},
			status:           StatusFail,
			evidenceContains: "process.command_args",
		},
		{
			name: "clean resource passes",
			fold: func(a *Accumulator) {
				a.AcceptTraces(traceReq(spansReq(goodResource("svc", "i", "p", "u"),
					span("t", "s", "", tracepb.Span_SPAN_KIND_SERVER, "x", ns(1), ns(6)))))
			},
			status: StatusPass,
		},
	})
}

func TestEvalSPA001(t *testing.T) {
	internal := func(traceID string, idx int) *tracepb.Span {
		return span(traceID, "in"+itoa(idx), "", tracepb.Span_SPAN_KIND_INTERNAL, "inner op", ns(1), ns(6))
	}
	runRuleCases(t, "SPA-001", []ruleCase{
		{
			name: "eleven internal spans in one trace fails",
			fold: func(a *Accumulator) {
				r := goodResource("svc", "i", "p", "u")
				var sps []*tracepb.Span
				for i := 0; i < 11; i++ {
					sps = append(sps, internal("t1", i))
				}
				a.AcceptTraces(traceReq(spansReq(r, sps...)))
			},
			status:           StatusFail,
			evidenceContains: "11 INTERNAL",
		},
		{
			name: "ten internal spans pass, spread across traces",
			fold: func(a *Accumulator) {
				r := goodResource("svc", "i", "p", "u")
				var sps []*tracepb.Span
				for i := 0; i < 10; i++ {
					sps = append(sps, internal("t"+itoa(i%2), i))
				}
				a.AcceptTraces(traceReq(spansReq(r, sps...)))
			},
			status: StatusPass,
		},
	})
}

func TestEvalSPA002(t *testing.T) {
	runRuleCases(t, "SPA-002", []ruleCase{
		{
			name: "orphan span fails",
			fold: func(a *Accumulator) {
				a.AcceptTraces(traceReq(spansReq(goodResource("svc", "i", "p", "u"),
					span("t1", "s1", "missingparent", tracepb.Span_SPAN_KIND_SERVER, "x", ns(1), ns(6)))))
			},
			status: StatusFail,
		},
		{
			name: "resolved parent passes",
			fold: func(a *Accumulator) {
				a.AcceptTraces(traceReq(spansReq(goodResource("svc", "i", "p", "u"),
					span("t1", "s1", "", tracepb.Span_SPAN_KIND_SERVER, "x", ns(1), ns(6)),
					span("t1", "s2", "s1", tracepb.Span_SPAN_KIND_CLIENT, "y", ns(1), ns(6)))))
			},
			status: StatusPass,
		},
		{
			name: "same parent id in another trace does not resolve",
			fold: func(a *Accumulator) {
				a.AcceptTraces(traceReq(spansReq(goodResource("svc", "i", "p", "u"),
					span("t1", "s1", "", tracepb.Span_SPAN_KIND_SERVER, "x", ns(1), ns(6)),
					span("t2", "s2", "s1", tracepb.Span_SPAN_KIND_SERVER, "y", ns(1), ns(6)))))
			},
			status: StatusFail,
		},
	})
}

func TestEvalSPA004(t *testing.T) {
	runRuleCases(t, "SPA-004", []ruleCase{
		{
			name: "client root span fails",
			fold: func(a *Accumulator) {
				a.AcceptTraces(traceReq(spansReq(goodResource("svc", "i", "p", "u"),
					span("t1", "s1", "", tracepb.Span_SPAN_KIND_CLIENT, "GET /api", ns(1), ns(6)))))
			},
			status: StatusFail,
		},
		{
			name: "server root span passes",
			fold: func(a *Accumulator) {
				a.AcceptTraces(traceReq(spansReq(goodResource("svc", "i", "p", "u"),
					span("t1", "s1", "", tracepb.Span_SPAN_KIND_SERVER, "GET /", ns(1), ns(6)))))
			},
			status: StatusPass,
		},
		{
			name: "client child span passes",
			fold: func(a *Accumulator) {
				a.AcceptTraces(traceReq(spansReq(goodResource("svc", "i", "p", "u"),
					span("t1", "s1", "", tracepb.Span_SPAN_KIND_SERVER, "GET /", ns(1), ns(6)),
					span("t1", "s2", "s1", tracepb.Span_SPAN_KIND_CLIENT, "GET /api", ns(1), ns(6)))))
			},
			status: StatusPass,
		},
	})
}

func TestEvalSPA005(t *testing.T) {
	short := func(traceID string, idx int) *tracepb.Span {
		return span(traceID, "sh"+itoa(idx), "", tracepb.Span_SPAN_KIND_INTERNAL, "fast", 0, 2_000_000) // 2ms
	}
	runRuleCases(t, "SPA-005", []ruleCase{
		{
			name: "twenty-one sub-5ms spans in one trace fail",
			fold: func(a *Accumulator) {
				r := goodResource("svc", "i", "p", "u")
				var sps []*tracepb.Span
				for i := 0; i < 21; i++ {
					sps = append(sps, short("t1", i))
				}
				a.AcceptTraces(traceReq(spansReq(r, sps...)))
			},
			status: StatusFail,
		},
		{
			name: "spread short spans pass",
			fold: func(a *Accumulator) {
				r := goodResource("svc", "i", "p", "u")
				var sps []*tracepb.Span
				for i := 0; i < 20; i++ {
					sps = append(sps, short("t"+itoa(i%2), i))
				}
				a.AcceptTraces(traceReq(spansReq(r, sps...)))
			},
			status: StatusPass,
		},
		{
			name: "five-millisecond boundary is not short",
			fold: func(a *Accumulator) {
				r := goodResource("svc", "i", "p", "u")
				var sps []*tracepb.Span
				for i := 0; i < 30; i++ {
					sps = append(sps, span("t1", "ex"+itoa(i), "", tracepb.Span_SPAN_KIND_INTERNAL, "exact", ns(0), ns(5_000_000)))
				}
				a.AcceptTraces(traceReq(spansReq(r, sps...)))
			},
			status: StatusPass,
		},
	})
}

func TestEvalSPA006(t *testing.T) {
	runRuleCases(t, "SPA-006", []ruleCase{
		{
			name: "sensitive db query fails (vendored example)",
			fold: func(a *Accumulator) {
				a.AcceptTraces(traceReq(spansReq(goodResource("svc", "i", "p", "u"),
					span("t1", "s1", "", tracepb.Span_SPAN_KIND_SERVER, "GET /users", ns(1), ns(6),
						kv("db.query.text", "SELECT * FROM users WHERE email='alice@example.com'")))))
			},
			status: StatusFail,
		},
		{
			name: "authorization header fails",
			fold: func(a *Accumulator) {
				a.AcceptTraces(traceReq(spansReq(goodResource("svc", "i", "p", "u"),
					span("t1", "s1", "", tracepb.Span_SPAN_KIND_SERVER, "GET /", ns(1), ns(6),
						kv("http.request.header.authorization", "Bearer eyJhbGciOiJIUzI1NiJ9.e30.sig")))))
			},
			status: StatusFail,
		},
		{
			name: "exception event with password fails",
			fold: func(a *Accumulator) {
				sp := span("t1", "s1", "", tracepb.Span_SPAN_KIND_SERVER, "GET /", ns(1), ns(6))
				sp.Events = []*tracepb.Span_Event{{
					Name:       "exception",
					Attributes: []*commonpb.KeyValue{kv("exception.message", "Login failed: password=hunter2, user=alice@example.com")},
				}}
				a.AcceptTraces(traceReq(spansReq(goodResource("svc", "i", "p", "u"), sp)))
			},
			status: StatusFail,
		},
		{
			name: "clean span passes",
			fold: func(a *Accumulator) {
				a.AcceptTraces(traceReq(spansReq(goodResource("svc", "i", "p", "u"),
					span("t1", "s1", "", tracepb.Span_SPAN_KIND_SERVER, "GET /users/42", ns(1), ns(6),
						kv("http.request.method", "GET")))))
			},
			status: StatusPass,
		},
	})
}

func TestNotEvaluatedRules(t *testing.T) {
	for _, id := range []string{"SPA-003", "SDK-001"} {
		v := evaluateRule(t, id, func(a *Accumulator) {
			a.AcceptTraces(traceReq(spansReq(goodResource("svc", "i", "p", "u"),
				span("t", "s", "", tracepb.Span_SPAN_KIND_SERVER, "x", ns(1), ns(6)))))
		})
		if v.Status != StatusNotEvaluated {
			t.Fatalf("%s: status %q, want not_evaluated (%s)", id, v.Status, v.Evidence)
		}
	}
}
