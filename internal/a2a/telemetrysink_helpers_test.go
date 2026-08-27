/*
Copyright 2026 The K8squad Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the limitations under the License.
*/

package a2a

import (
	"context"
	"testing"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/K8squad/K8squad/pkg/telemetry/toolusage"
)

// newSinkMapper builds a toolusage.Mapper over a real trace pipeline for
// sink tests. exp nil → no exporter (spans recorded nowhere; safe).
func newSinkMapper(t *testing.T, exp sdktrace.SpanExporter) *toolusage.Mapper {
	t.Helper()
	var opts []sdktrace.TracerProviderOption
	if exp != nil {
		// Short batch window so collector assertions do not wait the 5s
		// SDK default.
		opts = append(opts, sdktrace.WithBatcher(exp, sdktrace.WithBatchTimeout(50*time.Millisecond)))
	}
	tp := sdktrace.NewTracerProvider(opts...)
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	return toolusage.NewMapper(tp.Tracer("ksquad-test"), nil)
}
