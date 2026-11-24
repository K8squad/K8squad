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

package instscoreingest

import (
	"compress/gzip"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"io"
	"mime"
	"net/http"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	logscolpb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	metricscolpb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	tracecolpb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
)

// The ingestion route speaks the OTLP/HTTP protocol (the same wire contract
// the gateway's otlphttp exporters already use for the vendor backend), so the
// tap stays on the vanilla collector image: no custom component, just another
// exporter pointed here. Content types per the protocol: protobuf requests
// carry application/x-protobuf, JSON requests application/json; gzip request
// bodies are tolerated. Responses mirror the request encoding with the (empty)
// Export*ServiceResponse the protocol defines.

// maxBodyBytes bounds one mirrored batch. The gateway batches at
// send_batch_max_size 16384 records; at a few KB per span that leaves ample
// headroom under 64 MiB while still rejecting runaway bodies.
const maxBodyBytes = 64 << 20

const (
	contentTypeProtobuf = "application/x-protobuf"
	contentTypeJSON     = "application/json"
)

// Handler returns the OTLP/HTTP ingestion surface for the service:
// POST /v1/traces, /v1/metrics, /v1/logs. It carries its OWN bearer-token
// auth (the gateway collector has no console session): the token is the
// operator-configured KSQUAD_INSTSCORE_TOKEN, compared in constant time.
func (s *Service) Handler(token string) http.Handler {
	want := sha256.Sum256([]byte(token))
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/traces", s.authed(want, s.serveTraces))
	mux.HandleFunc("/v1/metrics", s.authed(want, s.serveMetrics))
	mux.HandleFunc("/v1/logs", s.authed(want, s.serveLogs))
	return mux
}

type otlpHandler func(w http.ResponseWriter, r *http.Request, body []byte, jsonEncoded bool)

func (s *Service) authed(want [sha256.Size]byte, next otlpHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		got := sha256.Sum256([]byte(bearerToken(r)))
		if subtle.ConstantTimeCompare(got[:], want[:]) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="instscore"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		contentType := r.Header.Get("Content-Type")
		mediaType := ""
		if contentType != "" {
			if mt, _, err := mime.ParseMediaType(contentType); err == nil {
				mediaType = mt
			}
		}
		var jsonEncoded bool
		switch mediaType {
		case contentTypeProtobuf:
			jsonEncoded = false
		case contentTypeJSON:
			jsonEncoded = true
		default:
			http.Error(w, "unsupported media type: use application/x-protobuf or application/json",
				http.StatusUnsupportedMediaType)
			return
		}

		body, err := readBody(w, r)
		if err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				http.Error(w, "batch too large", http.StatusRequestEntityTooLarge)
				return
			}
			http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
			return
		}
		next(w, r, body, jsonEncoded)
	}
}

func (s *Service) serveTraces(w http.ResponseWriter, r *http.Request, body []byte, jsonEncoded bool) {
	req := &tracecolpb.ExportTraceServiceRequest{}
	if !decodeOTLP(w, body, jsonEncoded, req) {
		return
	}
	if err := s.IngestTraces(r.Context(), req); err != nil {
		http.Error(w, "ingest failed", http.StatusServiceUnavailable)
		return
	}
	writeOTLPResponse(w, jsonEncoded, &tracecolpb.ExportTraceServiceResponse{})
}

func (s *Service) serveMetrics(w http.ResponseWriter, r *http.Request, body []byte, jsonEncoded bool) {
	req := &metricscolpb.ExportMetricsServiceRequest{}
	if !decodeOTLP(w, body, jsonEncoded, req) {
		return
	}
	if err := s.IngestMetrics(r.Context(), req); err != nil {
		http.Error(w, "ingest failed", http.StatusServiceUnavailable)
		return
	}
	writeOTLPResponse(w, jsonEncoded, &metricscolpb.ExportMetricsServiceResponse{})
}

func (s *Service) serveLogs(w http.ResponseWriter, r *http.Request, body []byte, jsonEncoded bool) {
	req := &logscolpb.ExportLogsServiceRequest{}
	if !decodeOTLP(w, body, jsonEncoded, req) {
		return
	}
	if err := s.IngestLogs(r.Context(), req); err != nil {
		http.Error(w, "ingest failed", http.StatusServiceUnavailable)
		return
	}
	writeOTLPResponse(w, jsonEncoded, &logscolpb.ExportLogsServiceResponse{})
}

// decodeOTLP unmarshals one export request per the negotiated encoding.
func decodeOTLP(w http.ResponseWriter, body []byte, jsonEncoded bool, m proto.Message) bool {
	var err error
	if jsonEncoded {
		err = protojson.UnmarshalOptions{DiscardUnknown: true}.Unmarshal(body, m)
	} else {
		err = proto.Unmarshal(body, m)
	}
	if err != nil {
		http.Error(w, "invalid OTLP payload: "+err.Error(), http.StatusBadRequest)
		return false
	}
	return true
}

// writeOTLPResponse answers with the protocol's (empty) response message in
// the request's encoding — the exporter treats non-2xx as retryable, so a
// clean 200 with a valid body closes the batch without retry loops.
func writeOTLPResponse(w http.ResponseWriter, jsonEncoded bool, m proto.Message) {
	if jsonEncoded {
		body, err := protojson.Marshal(m)
		if err != nil {
			http.Error(w, "encode response", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", contentTypeJSON)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
		return
	}
	body, err := proto.Marshal(m)
	if err != nil {
		http.Error(w, "encode response", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", contentTypeProtobuf)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// readBody reads the request body, transparently decompressing gzip and
// enforcing the size bound on the DEcompressed stream.
func readBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	src := http.MaxBytesReader(w, r.Body, maxBodyBytes)
	if r.Header.Get("Content-Encoding") == "gzip" {
		zr, err := gzip.NewReader(src)
		if err != nil {
			return nil, err
		}
		defer zr.Close()
		return io.ReadAll(zr)
	}
	return io.ReadAll(src)
}

func bearerToken(r *http.Request) string {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if len(h) > len(prefix) && h[:len(prefix)] == prefix {
		return h[len(prefix):]
	}
	return ""
}
