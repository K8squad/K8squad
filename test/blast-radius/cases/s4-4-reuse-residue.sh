#!/usr/bin/env bash
# S4-4 — reuse-residue (§9.3 / NFR-SEC5, F6/F7 / Epic X.2).
#
# Anchor 1:1 (residue_after_teardown): after teardown-and-replace, a fresh
# sandbox on the same per-principal PVC subPath exposes NO prior-Run
# scratch/secret/worktree; a sibling principal's subPath never sees it either.
#
#   conformance: run-a writes residue → teardown wipe runs → fresh run-a2
#                finds nothing; principal-b's subPath is independent.
#   mutation   : skip the teardown wipe → run-a2 MUST see the residue. If
#                residue does not survive, no teeth.
set -u
BASE=$(cd "$(dirname "$0")/.." && pwd)
# shellcheck source=lib.sh
. "$BASE/lib.sh"

NS_A="team-a${S4_SUFFIX:-}"
ARM="${S4_ARM:-conformance}"
PVC=run-scratch
MARKERS="scratch.tmp worktree.git secret.token"

spawn_run_pod() { # spawn_run_pod <name> <principal> <command>
  kubectl -n "$NS_A" delete pod "$1" --ignore-not-found --wait=true >/dev/null 2>&1
  cat <<EOF | kubectl apply -f -
apiVersion: v1
kind: Pod
metadata:
  name: $1
  namespace: ${NS_A}
  labels:
    ksquad.io/component: run-sandbox
spec:
  restartPolicy: Never
  containers:
    - name: run
      image: busybox:1.37
      command: ["sh", "-c", "$3"]
      volumeMounts:
        - name: scratch
          mountPath: /scratch
          subPath: $2
  volumes:
    - name: scratch
      persistentVolumeClaim:
        claimName: ${PVC}
EOF
}

wait_pod_phase() { # wait_pod_phase <name> <Succeeded|Failed>
  kubectl -n "$NS_A" wait --for="jsonpath={.status.phase}=$2" "pod/$1" --timeout=120s
}

teardown_wipe() { # the §9.3 guard: wipe the principal's subPath on teardown
  kubectl -n "$NS_A" delete job teardown-wipe --ignore-not-found --wait=true >/dev/null 2>&1
  cat <<EOF | kubectl apply -f -
apiVersion: batch/v1
kind: Job
metadata:
  name: teardown-wipe
  namespace: ${NS_A}
spec:
  backoffLimit: 0
  template:
    spec:
      restartPolicy: Never
      containers:
        - name: wipe
          image: busybox:1.37
          command: ["sh", "-c", "rm -rf /vol/principal-a/* /vol/principal-a/.[!.]* 2>/dev/null; echo wiped"]
          volumeMounts:
            - name: vol
              mountPath: /vol
              subPath: principal-a
      volumes:
        - name: vol
          persistentVolumeClaim:
            claimName: ${PVC}
EOF
  wait_job "$NS_A" teardown-wipe
}

if [ "$ARM" = "conformance" ]; then
  echo "S4-4 reuse-residue (conformance)"
  spawn_run_pod run-a principal-a \
    "echo residue >/scratch/scratch.tmp; mkdir -p /scratch/worktree.git; echo token >/scratch/secret.token; echo written"
  wait_pod_phase run-a Succeeded || { expect_pass "S4-4" "conformance" "run-a could not write residue markers" 1; exit 0; }

  # positive control: the residue IS on the subPath before teardown
  spawn_run_pod residue-check principal-a "test -f /scratch/secret.token"
  wait_pod_phase residue-check Succeeded
  residue_present=$?

  teardown_wipe
  wipe_ok=$?

  # fresh Run (teardown-and-REPLACE) on the same subPath must find nothing
  spawn_run_pod run-a2 principal-a 'ls -A /scratch | grep -q . && exit 1 || exit 0'
  wait_pod_phase run-a2 Succeeded
  clean_after_wipe=$?

  # sibling principal's subPath is a different address space
  spawn_run_pod run-b principal-b 'test -e /scratch/secret.token && exit 1 || exit 0'
  wait_pod_phase run-b Succeeded
  subpath_isolated=$?

  if [ "$residue_present" -eq 0 ] && [ "$wipe_ok" -eq 0 ] && [ "$clean_after_wipe" -eq 0 ] && [ "$subpath_isolated" -eq 0 ]; then
    expect_pass "S4-4" "conformance" \
      "residue written, teardown wiped, fresh Run clean, sibling principal subPath isolated" 0
  else
    [ "$residue_present" -ne 0 ] && bad "positive control failed — residue not observable before teardown"
    [ "$wipe_ok" -ne 0 ] && bad "teardown wipe job did not complete"
    [ "$clean_after_wipe" -ne 0 ] && bad "fresh Run found prior-Run residue after teardown (NFR-SEC5 leak)"
    [ "$subpath_isolated" -ne 0 ] && bad "principal-b subPath saw principal-a files (cross-principal PVC leak)"
    expect_pass "S4-4" "conformance" \
      "pre=$residue_present wipe=$wipe_ok clean=$clean_after_wipe iso=$subpath_isolated" 1
  fi
else
  echo "S4-4 reuse-residue (mutation: teardown wipe SKIPPED)"
  spawn_run_pod run-a-mut principal-a \
    "echo residue >/scratch/scratch.tmp; echo token >/scratch/secret.token; echo written"
  wait_pod_phase run-a-mut Succeeded
  wrote=$?
  # guard skipped: NO teardown_wipe call — that IS the mutation (§9.3 skip-the-wipe)
  spawn_run_pod run-a2-mut principal-a "test -f /scratch/secret.token"
  wait_pod_phase run-a2-mut Succeeded
  residue_visible=$?
  if [ "$wrote" -eq 0 ] && [ "$residue_visible" -eq 0 ]; then
    expect_escape "S4-4" "wipe skipped → fresh Run inherits prior-Run residue (secret.token readable)" 0
  else
    [ "$wrote" -ne 0 ] && bad "could not seed residue — fixture broken"
    [ "$residue_visible" -ne 0 ] && bad "residue did NOT survive without the wipe — no teeth"
    expect_escape "S4-4" "wrote=$wrote residue_visible=$residue_visible" 1
  fi
fi
