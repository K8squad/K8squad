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

package main

// read.go is the 8.7b (ISI-2903) shim live read verb: the sidecar co-located with a Run's live
// worktree answers read-only build-browser queries (tree | diff | file | meta) against that
// workspace and prints them as JSON with live:true. It is the LIVE half of the build browser — while
// the Run is executing its pod is the only place the worktree exists, so the apiserver reaches it
// through this verb (`shim read …`); once the Run completes and the pod is gone, the console falls
// back to the 8.7c build-snapshot (live:false) instead.
//
// The verb reuses the exact same git-plumbing reader (internal/buildbrowser.GitReader) the apiserver
// serves 8.7a with, so a live read and a snapshot read are byte-for-byte the same shape. It applies NO
// authorization — that is the BFF's 8.7d gate, upstream; the shim only ever reads its own workspace,
// which the reconciler already scoped to this Run.

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/K8squad/K8squad/internal/buildbrowser"
	"github.com/google/uuid"
)

// readEnvelope is the JSON the read verb emits: the requested resource, the resolved ref, live:true,
// and the reader result. live is stamped by the producer so a downstream consumer never has to infer
// whether a build view came from the worktree or a snapshot.
type readEnvelope struct {
	Live     bool        `json:"live"`
	Resource string      `json:"resource"`
	Ref      string      `json:"ref,omitempty"`
	Path     string      `json:"path,omitempty"`
	Result   interface{} `json:"result"`
}

// driveRead parses `read <resource> [--ref run|base] [--path P]`, runs the matching GitReader query
// against the Run's workspace, and writes the JSON envelope to stdout.
func driveRead(args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("read: missing resource (want: tree | diff | file | meta)")
	}
	// The resource is the leading positional; flags follow it. Go's flag package stops at the first
	// non-flag arg, so parse the flags from AFTER the resource.
	resource := args[0]
	fs := flag.NewFlagSet("read", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	ref := fs.String("ref", "run", "ref to read: run | base")
	path := fs.String("path", "", "file path (required for `file`)")
	if err := fs.Parse(args[1:]); err != nil {
		return fmt.Errorf("read: %w", err)
	}

	m := runMetaFromEnv()
	reader := buildbrowser.NewGitReader()
	ctx := context.Background()

	var (
		result interface{}
		err    error
	)
	switch resource {
	case "tree":
		result, err = reader.Tree(ctx, m, *ref)
	case "diff":
		result, err = reader.Diff(ctx, m)
	case "file":
		if *path == "" {
			return fmt.Errorf("read file: --path is required")
		}
		result, err = reader.File(ctx, m, *ref, *path)
	case "meta":
		result, err = reader.Meta(ctx, m)
	default:
		return fmt.Errorf("read: unknown resource %q (want: tree | diff | file | meta)", resource)
	}
	if err != nil {
		return fmt.Errorf("read %s: %w", resource, err)
	}

	env := readEnvelope{Live: true, Resource: resource, Result: result}
	if resource != "diff" && resource != "meta" {
		env.Ref = *ref
	}
	if resource == "file" {
		env.Path = *path
	}
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(env)
}

// runMetaFromEnv builds the read target from the environment the reconciler injects (arch §7.2/§7.3).
// The workspace path, run ref and base ref are server-controlled; TeamID/Principal are left zero
// because the shim applies no gate (the BFF's 8.7d does, upstream). Sensible defaults keep the verb
// runnable in a plain checkout during dev.
func runMetaFromEnv() buildbrowser.RunMeta {
	return buildbrowser.RunMeta{
		RunID:     os.Getenv("KSQUAD_RUN_ID"),
		TeamID:    uuid.Nil,
		Principal: "",
		RepoPath:  envOr("KSQUAD_WORKDIR", "."),
		HeadRef:   envOr("KSQUAD_RUN_REF", "HEAD"),
		BaseRef:   envOr("KSQUAD_BASE_REF", "origin/main"),
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
