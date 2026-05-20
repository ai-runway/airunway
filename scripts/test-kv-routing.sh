#!/usr/bin/env bash
# test-kv-routing.sh — smoke-test KV-aware routing for a ModelDeployment.
# NOTE: This script is meant for demonstrating and validating KV-aware routing 
# in a live cluster. Can be used for e2e once GPU resources are available.
#
# REQUIRED SETUP for this test to be meaningful:
#   1. AGGREGATED mode (spec.serving.mode: aggregated)
#        Disagg mode muddies the signal — the decode pod always receives KV
#        blocks via NIXL from a prefill peer, so "external prefix cache hit
#        rate" trivially reads ~100% and doesn't tell you anything about
#        cross-request routing decisions.
#   2. ≥2 worker pods (spec.scaling.replicas: 2+)
#        With a single pod there's no routing decision to make — all traffic
#        lands on the one pod trivially. You can only observe caching, not
#        routing.
#   3. KV-aware router mode enabled
#        The provider's router must be configured for prefix-aware routing,
#        not round-robin. How to set this depends on the provider — e.g.
#        for Dynamo set spec.provider.overrides.routerMode to "kv".
#        Without this, the test measures plain round-robin + on-pod prefix
#        caching — not KV-aware routing.
#
# What this test proves (given the setup above):
#   The provider's KV-aware router picks the worker that is most likely to
#   already hold KV-cache blocks for a given prompt prefix. A successful
#   KV-aware routing hit has two observable effects:
#
#     1. LATENCY: the second request with the same prompt is much faster
#        than the first — prefill recompute is skipped on the chosen pod.
#     2. PREFIX CACHE REUSE: the worker that serves A2 reports a high
#        "Prefix cache hit rate" for the window, because it's the same pod
#        that served A1 and still has the blocks resident.
#
# What it does:
#   1. Resolves the gateway endpoint and model name from the ModelDeployment.
#   2. Phase 1 — Cold requests (concurrent):
#        Sends A1, B1, C1, D1 simultaneously. Four distinct prompts hit the
#        router at once, creating queue pressure that forces distribution
#        across workers. Without concurrency, the router has no reason to
#        move off the first (warm-cache) worker.
#   3. Phase 2 — Warm requests (sequential):
#        Sends A2, B2, C2, D2 one at a time. Each repeats the prompt from
#        Phase 1. The KV router should send each back to the same worker
#        that cached the corresponding cold prompt.
#   4. Reports latency and target pod for each request.
#      X2 should be faster than X1 (cache hit) and land on the same pod.
#      With ≥2 workers, different prompt families (A vs B vs C vs D) should
#      distribute across workers — demonstrating the router distributes by
#      prefix, not randomly.
#   5. Reports the worker pod's own vLLM "Prefix cache hit rate" after the
#      run — this is the cross-request reuse signal.
#
# Usage:
#   ./scripts/test-kv-routing.sh <md-name> <md-namespace>
#
# Requires:
#   - kubectl configured against the target cluster
#   - jq, curl
#   - The ModelDeployment to be Running and HTTPRoute created

set -euo pipefail

MD_NAME="${1:?usage: $0 <md-name> <md-namespace>}"
MD_NAMESPACE="${2:?usage: $0 <md-name> <md-namespace>}"

echo "==> Resolving gateway endpoint and model name..."
ENDPOINT="${ENDPOINT:-$(kubectl get modeldeployment -n "$MD_NAMESPACE" "$MD_NAME" \
  -o jsonpath='{.status.gateway.endpoint}')}"
MODEL_NAME="${MODEL_NAME:-$(kubectl get modeldeployment -n "$MD_NAMESPACE" "$MD_NAME" \
  -o jsonpath='{.status.gateway.modelName}')}"

if [[ -z "$ENDPOINT" || -z "$MODEL_NAME" ]]; then
  echo "ERROR: could not resolve gateway endpoint/model name from MD status" >&2
  echo "       .status.gateway must be populated — is the MD Running?" >&2
  echo "       Or set ENDPOINT and MODEL_NAME env vars to override." >&2
  exit 1
fi

echo "    endpoint:   $ENDPOINT"
echo "    modelName:  $MODEL_NAME"

PROVIDER="${PROVIDER:-$(kubectl get modeldeployment -n "$MD_NAMESPACE" "$MD_NAME" \
  -o jsonpath='{.status.provider.name}' 2>/dev/null)}"
echo "    provider:   ${PROVIDER:-(unknown)}"

# ── Worker pod discovery ──────────────────────────────────────────────
# Try provider-specific labels first, then fall back to generic strategies.
# Each provider labels its inference-serving pods differently.
WORKER_NAMESPACE="$MD_NAMESPACE"
WORKERS=""

discover_workers() {
  local label="$1"
  kubectl get pods -n "$WORKER_NAMESPACE" -l "$label" \
    --field-selector=status.phase=Running \
    -o jsonpath='{.items[*].metadata.name}' 2>/dev/null | tr ' ' '\n'
}

case "$PROVIDER" in
  dynamo)
    # Dynamo labels workers with the DGD name and component type.
    # Workers live in the same namespace as the ModelDeployment, but if the MD
    # namespace differs from dynamo-system, also check dynamo-system as a fallback.
    WORKERS=$(discover_workers "nvidia.com/dynamo-graph-deployment-name=$MD_NAME" \
      | grep -i worker || true)
    if [[ -z "$WORKERS" && "$WORKER_NAMESPACE" != "dynamo-system" ]]; then
      WORKER_NAMESPACE="dynamo-system"
      WORKERS=$(discover_workers "nvidia.com/dynamo-graph-deployment-name=$MD_NAME" \
        | grep -i worker || true)
    fi
    ;;
  kaito)
    # KAITO uses workspace-based labeling
    WORKERS=$(discover_workers "kaito.sh/workspace=$MD_NAME" || true)
    ;;
  llmd)
    # llm-d labels worker pods with the deployment name
    WORKERS=$(discover_workers "app.kubernetes.io/instance=$MD_NAME" \
      | grep -vi -e epp -e router -e frontend || true)
    ;;
esac

# Generic fallback: look for pods with the airunway managed-by label
if [[ -z "$WORKERS" ]]; then
  WORKERS=$(discover_workers "app.kubernetes.io/instance=$MD_NAME" \
    | grep -vi -e epp -e router -e frontend -e gateway || true)
fi

if [[ -z "$WORKERS" ]]; then
  echo "ERROR: no worker pods found for $MD_NAME in $WORKER_NAMESPACE" >&2
  echo "       Detected provider: ${PROVIDER:-(none)}. Check pod labels." >&2
  exit 1
fi

echo
NUM_WORKERS=$(echo "$WORKERS" | wc -w | tr -d ' ')
echo "==> Found $NUM_WORKERS worker pod(s):"
for w in $WORKERS; do echo "      - $w"; done
if [[ "$NUM_WORKERS" -lt 2 ]]; then
  echo
  echo "WARNING: only $NUM_WORKERS worker pod(s) detected. KV-aware routing needs"
  echo "         >=2 workers to make a meaningful routing decision — with a single"
  echo "         pod you will only observe on-pod prefix caching, not routing."
fi

# ── Build pod IP → name map ──────────────────────────────────────────
# Used to resolve EPP log endpoints (IPs) back to pod names.
declare -A IP_TO_POD
for w in $WORKERS; do
  ip=$(kubectl get pod -n "$WORKER_NAMESPACE" "$w" -o jsonpath='{.status.podIP}' 2>/dev/null || true)
  if [[ -n "$ip" ]]; then
    IP_TO_POD["$ip"]="$w"
  fi
done

if [[ -n "${EPP_POD:-}" ]]; then
  echo "==> EPP pod: $EPP_POD"
fi

# Cut-off timestamp for log scraping (ignore anything earlier).
SINCE=$(date -u +"%Y-%m-%dT%H:%M:%SZ")

# ── Metrics-based pod detection ──────────────────────────────────────
# The gateway may send requests to any pod (EPP returns uniform scores),
# but the sidecar forwards over Dynamo RPC to the actual target worker.
# To find which worker actually processed the request, we scrape
# vllm:prompt_tokens_total from each worker before and after the request.
# The worker whose counter increased is the one that did the work.
METRICS_PORT=9090

get_prompt_tokens() {
  local pod="$1"
  kubectl exec -n "$WORKER_NAMESPACE" "$pod" -c main -- \
    curl -s "localhost:${METRICS_PORT}/metrics" 2>/dev/null \
    | grep '^vllm:prompt_tokens_total' \
    | grep -oE '[0-9.]+$' || echo "0"
}

# fire_request <label> <prompt> <tmpdir> — sends one request, records latency.
# Does NOT detect pod (caller handles that separately).
fire_request() {
  local label="$1" prompt="$2" tmpdir="$3"
  jq -nc --arg model "$MODEL_NAME" --arg prompt "$prompt" '{
    model: $model, max_tokens: 16, temperature: 0,
    messages: [{role: "user", content: $prompt}]
  }' | curl -sS -X POST "http://$ENDPOINT/v1/chat/completions" \
    -H 'Content-Type: application/json' \
    -H "X-Gateway-Model-Name: $MODEL_NAME" \
    -d @- \
    -o "$tmpdir/$label.body" \
    -w '%{time_total}' > "$tmpdir/$label.latency"
}

# detect_pod <label> <tmpdir> — uses per-request before/after metrics snapshots
# to determine which worker processed the request.
detect_pod() {
  local label="$1" tmpdir="$2"
  sleep 1  # let metrics flush
  local pod_name="unknown"
  local max_delta=0
  for w in $WORKERS; do
    local after_tok
    after_tok=$(get_prompt_tokens "$w")
    local before_tok
    before_tok=$(cat "$tmpdir/$label.before.$w")
    local delta
    delta=$(echo "$after_tok - $before_tok" | bc 2>/dev/null || echo "0")
    if (( $(echo "$delta > $max_delta" | bc 2>/dev/null) )); then
      max_delta="$delta"
      pod_name="$w"
    fi
  done
  echo "$pod_name" > "$tmpdir/$label.pod"
}

# snapshot_tokens <tmpdir> <prefix> — saves current prompt_tokens for each worker.
snapshot_tokens() {
  local tmpdir="$1" prefix="$2"
  for w in $WORKERS; do
    get_prompt_tokens "$w" > "$tmpdir/${prefix}.before.$w"
  done
}

# send_request <label> <prompt> <tmpdir> — sequential send with per-request pod detection.
send_request() {
  local label="$1" prompt="$2" tmpdir="$3"
  echo
  echo "==> Sending $label"
  snapshot_tokens "$tmpdir" "$label"
  fire_request "$label" "$prompt" "$tmpdir"
  local lat
  lat=$(cat "$tmpdir/$label.latency")
  detect_pod "$label" "$tmpdir"
  printf "    latency: %ss   pod: %s\n" "$lat" "$(cat "$tmpdir/$label.pod")"
}

# Prompts must be long enough that prefill dominates total latency. On H100
# with a 7B model, ~500 tokens still finishes in single-digit milliseconds —
# lost in the ~180ms of fixed overhead (HTTP, routing, decode). We need ~4K+
# tokens so prefill takes hundreds of milliseconds, making the A1→A2 delta
# from prefix cache reuse unmistakable.
#
# PROMPT_A and PROMPT_B share NO meaningful prefix (beyond BOS / chat template)
# so B1 cannot benefit from A1's cached blocks.
read -r -d '' PROMPT_A << 'PROMPT_EOF' || true
You are a distributed systems expert. I need an exhaustive technical explanation covering all of the following topics in a single, cohesive response. Be extremely thorough — for each topic, include the full protocol specification, correctness invariants, failure modes, and performance characteristics.

1. The Raft consensus algorithm:
   a. Leader election: Explain the state machine (follower, candidate, leader), randomized election timeouts and why they prevent split-vote livelock, the RequestVote RPC including term comparison and log-up-to-date check, what happens when two candidates start an election simultaneously, and how a candidate handles receiving an AppendEntries from a valid leader during its election. Explain the role of the pre-vote extension and why it prevents disruptions from partitioned servers rejoining.
   b. Log replication: Explain how the leader maintains nextIndex and matchIndex per follower, the AppendEntries RPC including the consistency check (prevLogIndex, prevLogTerm), the fast log backtracking optimization (conflictIndex, conflictTerm), batching of entries, and flow control. Explain how the leader decides when an entry is committed (majority matchIndex), and the subtle rule that a leader cannot commit entries from previous terms using the current term's commit index — illustrate this with the specific counterexample from Section 5.4.2 of the Raft paper where committing old-term entries leads to safety violations.
   c. Persistence and crash recovery: Which state must be persisted before responding to any RPC (currentTerm, votedFor, log[])? Why is votedFor persistence critical for safety? What happens if a server crashes after persisting a vote but before sending the response?
   d. Cluster membership changes: Explain the joint consensus approach, the single-server change simplification, and the safety argument for why at most one configuration change can be pending at a time. Explain the AddServer and RemoveServer RPCs and the leadership transfer extension.
   e. Log compaction and snapshots: Explain how snapshots replace prefix segments of the log, the InstallSnapshot RPC, and the interaction between snapshots and slow followers that fall behind.
   f. Linearizable reads: Explain the three approaches — leader leases with clock assumptions, read-index with a heartbeat round, and log-reads where the read is appended to the log. Analyze the trade-offs of each.

2. The Viewstamped Replication Revisited protocol:
   a. Normal operation: Explain the role of the primary, the op-number, commit-number, and view-number. How does the primary assign op-numbers, broadcast Prepare messages, collect PrepareOk responses, and advance the commit point? How do backups apply committed operations to their state machines?
   b. View change: Explain the StartViewChange and DoViewChange messages in detail. How does the new primary determine the correct log state from the set of DoViewChange messages it receives? What is the significance of the view-number in each DoViewChange? Explain the subtle case where the new primary's log is shorter than a backup's log — how is this resolved safely?
   c. Recovery: Explain the Recovery and RecoveryResponse protocol that allows a crashed replica to rejoin. Why must the recovering replica obtain a new nonce? Why must it wait for a response from the current primary specifically, not just any replica?
   d. Reconfiguration: Explain the epoch-based reconfiguration mechanism, including the StartEpoch message and how it interacts with in-progress view changes.

3. Chain Replication and CRAQ:
   a. Basic chain replication: Explain the topology (head, intermediate nodes, tail), write propagation from head to tail, read serving at the tail only, and why this gives strong consistency. Analyze the steady-state latency (write latency = chain length * inter-node RTT, read latency = 1 RTT to tail) and throughput characteristics (write throughput limited by slowest link, read throughput scales with tail capacity only).
   b. Failure handling: Explain how the chain master detects and handles head failure (promote successor), tail failure (predecessor becomes tail, re-sends pending writes), and mid-chain failure (predecessor links to successor, re-sends in-flight writes). Explain the "sent" vs "received" bookkeeping that makes mid-chain repair correct.
   c. CRAQ (Chain Replication with Apportioned Queries): Explain the clean/dirty version distinction, how any replica can serve reads for clean objects (all versions committed), and how dirty reads require a version check against the tail. Analyze how this improves read throughput by distributing reads across all replicas while maintaining strong consistency.
   d. Chain replication for transactions: Explain the Sinfonia mini-transaction protocol that uses chain replication for individual items, and how TAPIR combines this with inconsistent replication for cross-shard transactions.

4. Comparison across all three protocols, analyzing: (a) read and write latency in the common case and under contention, (b) behavior during asymmetric network partitions where the leader/primary/head can reach some replicas but not others, (c) the exact conditions under which each protocol becomes unavailable (minority partition, simultaneous failures exceeding f in a 2f+1 cluster), (d) reconfiguration complexity and whether it requires stopping all operations, (e) suitability for geo-distributed deployments where inter-replica RTT is 50-200ms, and (f) how each protocol handles slow replicas (one node 10x slower than others) without sacrificing availability.

Be precise and technical. Include pseudocode for all key protocol steps.
PROMPT_EOF

read -r -d '' PROMPT_B << 'PROMPT_EOF' || true
You are a database internals expert. I need an exhaustive technical explanation covering all of the following topics in a single, cohesive response. Be extremely thorough — for each topic, include the full algorithm specification, correctness invariants, failure modes, and performance characteristics.

1. Write-Ahead Logging (WAL) and ARIES recovery:
   a. WAL fundamentals: Explain the write-ahead logging protocol — why the log record for a modification must be flushed to stable storage before the modified data page can be written. Define the WAL record structure: LSN (Log Sequence Number), prevLSN (linking records for the same transaction), transactionID, pageID, offset, before-image, after-image. Explain physiological logging (physical-to-a-page, logical-within-a-page) and why it reduces log volume compared to pure physical logging while avoiding the complications of pure logical logging during undo.
   b. Buffer pool interaction: Explain the pageLSN stored on each data page, the flushedLSN maintained by the log manager, and the no-force/steal buffer management policy that ARIES enables. Explain why the steal policy (allowing dirty pages to be written before commit) requires undo capability, and why the no-force policy (not requiring all dirty pages to be flushed at commit) requires redo capability. Explain the checkpoint protocol: begin_checkpoint, end_checkpoint, the active transaction table (ATT), and the dirty page table (DPT).
   c. ARIES recovery algorithm: Explain the three passes in complete detail. Analysis pass: scan forward from the last checkpoint's begin_checkpoint LSN, reconstruct the ATT and DPT, identify the starting point for redo (minimum recLSN in DPT). Redo pass: scan forward from the redo starting point, for each redo-able log record check whether the page's pageLSN is less than the record's LSN (if so, re-apply the action and update pageLSN). Undo pass: process the loser transactions (those in ATT at end of analysis that did not commit), undo their actions in reverse LSN order using the prevLSN chain, and write CLRs (Compensation Log Records) for each undone action. Explain why CLRs have an undoNextLSN that points past the record being compensated, and how this prevents repeated undo work if the system crashes during recovery.
   d. Group commit: Explain how the log manager batches multiple transaction commits into a single fsync to amortize I/O cost, the trade-off between commit latency and throughput, and how the commit queue and wake-up mechanism work. Explain the interaction with write-behind (asynchronous page flushing) and how the buffer pool's page cleaner coordinates with the WAL flush position.
   e. WAL in modern systems: Explain how WBL (Write-Behind Logging) in NVM-aware databases (e.g., Peloton) inverts the traditional approach by flushing dirty pages first and using the log only for undo. Explain the SILK approach for reducing WAL I/O interference with data I/O on SSDs.

2. Multi-Version Concurrency Control (MVCC) in depth:
   a. Append-only / copy-on-write MVCC (PostgreSQL model): Explain how each UPDATE creates a new physical tuple version in the heap, linked via the t_ctid chain. Explain the visibility rules using xmin, xmax, and the CLOG (commit log). Explain VACUUM — why it is necessary (dead tuples accumulate), how it identifies reclaimable tuples, the FREEZE operation that prevents transaction ID wraparound, and the autovacuum launcher/worker architecture. Explain the HOT (Heap-Only Tuple) optimization that avoids index updates when the new tuple fits on the same page and no indexed column changes.
   b. Undo-log MVCC (InnoDB model): Explain how the latest version lives in-place in the clustered index, with previous versions reconstructed from the undo log (rollback segment). Explain the purge system that reclaims undo log entries once no active transaction can need them. Explain the read view (consistent snapshot) mechanism: the list of active transaction IDs at snapshot creation time, and the visibility algorithm that checks whether a row's trx_id is committed and visible to the snapshot.
   c. Snapshot Isolation (SI): Formally define SI using the "First-Committer-Wins" rule. Explain the write skew anomaly with the classic doctor-on-call example (two doctors each check that the other is on call, then both go off call). Explain why SI is not serializable and give the exact characterization of the anomalies SI permits (the dangerous structure is two consecutive rw-anti-dependency edges forming a cycle).
   d. Serializable Snapshot Isolation (SSI): Explain how SSI tracks rw-anti-dependencies between concurrent transactions (both incoming and outgoing), detects dangerous structures (two consecutive rw-anti-dependencies involving a pivot transaction), and aborts one of the involved transactions to break the cycle. Explain the false positive rate and the heuristics used to reduce unnecessary aborts (e.g., the committed transaction optimization, the read-only transaction optimization).
   e. MVCC for non-relational systems: Explain how FoundationDB implements MVCC using its ordered key-value store with versionstamp keys, and how CockroachDB's MVCC layer uses hybrid-logical clocks (HLC) to timestamp versions and resolve clock skew across nodes.

3. B-Tree and B+Tree concurrency in depth:
   a. Basic B+Tree structure: Explain the distinction between B-trees and B+trees (data in leaf nodes only), the search algorithm, and the properties maintained by splits and merges. Explain the fill factor, the difference between leaf splits (redistribute keys) and internal node splits (push up the middle key), and the cascading nature of splits.
   b. Latch-coupling (crabbing): Explain the protocol for concurrent traversal — acquire latch on child before releasing latch on parent. For read operations: shared latches, released immediately on the way down. For write operations: exclusive latches, held until it is confirmed that no split/merge will propagate upward (safe node = node that will not split or merge after the insertion/deletion). Explain why you release all ancestor latches once you reach a safe node.
   c. Optimistic latching (B-link trees): Explain Lehman and Yao's B-link tree that adds right-link pointers and high-key values to handle concurrent splits. Explain the "move right" protocol when a search reaches a node that has been split. Explain the optimistic write path: traverse with read latches only, latch the leaf exclusively, and if a split propagates, restart from the root with pessimistic latch-coupling.
   d. Latch-free approaches: Explain the Bw-tree (used in SQL Server Hekaton and Azure Cosmos DB): the mapping table that provides indirection between logical and physical page pointers, delta chains that represent modifications without in-place updates, page consolidation that merges deltas into a new base page, and the structure modification protocol using CAS on the mapping table. Explain the OLFIT (Optimistic Latch-Free Index Traversal) approach using version numbers. Explain the ART (Adaptive Radix Tree) and how it achieves concurrency with optimistic lock coupling.
   e. Modern optimizations: Explain FAST (Fast Architecture Sensitive Tree) and its use of SIMD for intra-node search, cache-line-conscious node sizing, and the effect of hardware prefetching on B-tree traversal performance. Explain how persistent memory (Intel Optane) affects B-tree design — the need for 8-byte atomic writes, cache line flush ordering (CLWB + SFENCE), and the WORT (Write Optimal Radix Tree) design.

4. Compare the trade-offs in depth: (a) WAL-based recovery vs shadow paging (copy-on-write with page table swap) — analyze space amplification, recovery time, steady-state write amplification, and interaction with the buffer pool; (b) append-only MVCC vs undo-log MVCC — analyze heap bloat and vacuum overhead vs undo log management and purge overhead, and the impact on index maintenance; (c) latch-based B-trees vs latch-free approaches — analyze throughput under high contention (>64 cores), tail latency from latch convoys, memory management challenges (epoch-based reclamation in latch-free structures), and the engineering complexity trade-off; (d) how modern NVMe SSDs (with 4K page size, ~10us latency, ~1M IOPS) and persistent memory (~300ns latency, byte-addressable) fundamentally change the balance between compute and I/O in all three areas.

Be precise and technical. Include pseudocode for all key algorithm steps.
PROMPT_EOF

read -r -d '' PROMPT_C << 'PROMPT_EOF' || true
You are a networking expert. I need an exhaustive technical explanation covering all of the following topics in a single, cohesive response. Be extremely thorough — for each topic, include the full protocol specification, correctness invariants, failure modes, and performance characteristics.

1. TCP congestion control in depth:
   a. Classic TCP Reno and NewReno: Explain slow start (exponential growth of cwnd until ssthresh), congestion avoidance (additive increase of cwnd by 1 MSS per RTT), fast retransmit (triple duplicate ACK triggers retransmit without waiting for RTO), and fast recovery (NewReno's partial ACK handling that avoids re-entering slow start). Explain the AIMD (Additive Increase Multiplicative Decrease) principle and why it converges to fairness. Derive the TCP throughput equation: throughput ≈ (MSS/RTT) * (1/sqrt(p)) where p is the loss probability.
   b. TCP CUBIC: Explain the cubic function W(t) = C(t-K)^3 + W_max, where K = cbrt(W_max * beta / C), and how it produces concave growth after a loss (fast recovery toward W_max) followed by convex probing (aggressive growth beyond W_max). Explain the TCP-friendly region that ensures CUBIC doesn't starve Reno flows. Explain hystart++ for improved slow-start exit detection using ACK train and delay increase signals.
   c. BBR (Bottleneck Bandwidth and Round-trip propagation time): Explain the four phases — Startup (exponential probing for bandwidth), Drain (reduce inflight to BDP), ProbeBW (steady-state with 1.25x/0.75x gain cycling), and ProbeRTT (periodic cwnd drain to measure minimum RTT). Explain the BBR model: delivery_rate estimation, RTprop tracking with a 10-second window, and the inflight cap at 2*BDP. Explain BBRv2 improvements: loss-aware bandwidth probing, explicit congestion signaling, and the ecn_alpha parameter.
   d. QUIC and multipath: Explain how QUIC implements congestion control per-path, the interaction between stream-level flow control and connection-level flow control, and how 0-RTT resumption affects congestion state. Explain MPQUIC's path scheduling algorithms (round-robin, lowest-RTT, redundant) and how they interact with per-path congestion controllers.

2. Software-defined networking and programmable dataplanes:
   a. OpenFlow and the SDN control plane: Explain the match-action pipeline in OpenFlow switches, the flow table structure (priority, match fields, instructions, counters, timeouts), the role of the controller (reactive vs proactive flow installation), and the consistency challenges when updating distributed flow tables. Explain the Frenetic/NetKAT approach to provably correct network updates using two-phase commit.
   b. P4 and programmable ASICs: Explain the P4 language model — parser, match-action pipeline, deparser — and how it maps to the PISA (Protocol-Independent Switch Architecture) with configurable parser graph, multiple match-action stages, and stateful ALUs. Explain P4Runtime for control-plane interaction. Explain the constraints of hardware targets: limited stages, TCAM width, stateful memory per stage, and recirculation as an escape hatch.
   c. eBPF/XDP for programmable host networking: Explain the eBPF verifier (bounded loops, memory safety, helper function interface), the XDP hook point (before sk_buff allocation for zero-copy fast path), TC hook (after sk_buff for full protocol access), and the map abstraction for state sharing between eBPF programs and userspace. Explain Cilium's use of eBPF for Kubernetes networking: per-pod policy enforcement, service load balancing via BPF_MAP_TYPE_LRU_HASH, and transparent encryption using IPsec or WireGuard in eBPF.

3. Modern load balancing at scale:
   a. L4 load balancing: Explain Maglev's consistent hashing (the lookup table construction algorithm, disruption score minimization, and connection draining on backend changes). Explain ECMP-based DSR (Direct Server Return) where the load balancer only sees the SYN and the backend responds directly. Explain the interaction with TCP connection tracking and the need for consistent hashing to handle asymmetric routing.
   b. L7 load balancing and service mesh: Explain Envoy's architecture — the listener/filter chain model, the HCM (HTTP Connection Manager), route tables, cluster managers, and the xDS API (LDS, RDS, CDS, EDS, SDS) for dynamic configuration. Explain the ext_proc filter and how it enables external processing (as used by Gateway API Inference Extension). Explain the circuit breaking, outlier detection, and retry budget mechanisms.

4. Compare: (a) loss-based vs delay-based vs model-based congestion control under bufferbloat, (b) hardware vs software dataplanes for latency-sensitive workloads, (c) L4 vs L7 load balancing for long-lived streaming connections like LLM inference.

Be precise and technical. Include pseudocode for all key algorithm steps.
PROMPT_EOF

read -r -d '' PROMPT_D << 'PROMPT_EOF' || true
You are an operating systems expert. I need an exhaustive technical explanation covering all of the following topics in a single, cohesive response. Be extremely thorough — for each topic, include the full algorithm specification, correctness invariants, failure modes, and performance characteristics.

1. Virtual memory and page table management:
   a. Multi-level page tables: Explain the 4-level (PGD, PUD, PMD, PTE) and 5-level page table structures used in x86-64 Linux. Explain the trade-offs between page table depth and memory overhead. Explain huge pages (2MB and 1GB) and transparent huge pages (THP) — the khugepaged kernel thread, compaction, and the split/collapse lifecycle. Explain the performance implications: TLB coverage (a single 2MB TLB entry covers 512x more memory than a 4KB entry), page walk cost reduction, and the defragmentation overhead.
   b. TLB management: Explain the TLB hierarchy (L1 iTLB, L1 dTLB, L2 STLB), TLB shootdown via IPI (inter-processor interrupt) for maintaining coherence across cores, and the PCID (Process Context ID) optimization that avoids full TLB flushes on context switch. Explain ASID (Address Space ID) on ARM64 and how it differs from PCID. Explain the performance cost of TLB shootdowns in NUMA systems where IPI latency crosses socket boundaries.
   c. Memory-mapped I/O and page fault handling: Explain the Linux page fault path — do_page_fault, handle_mm_fault, the distinction between minor faults (page in page cache, just needs PTE update) and major faults (page must be read from disk). Explain demand paging, copy-on-write (COW) fork semantics, and the userfaultfd mechanism for userspace page fault handling (used by live migration and garbage collectors).
   d. IOMMU and device memory: Explain how the IOMMU (VT-d on Intel, AMD-Vi) provides DMA remapping for device isolation, the IOMMU page table structure, and the VFIO framework for safe device passthrough to VMs and containers. Explain the interaction between IOMMU and GPU memory management (unified virtual addressing, page migration between CPU and GPU).

2. Process scheduling on modern hardware:
   a. CFS (Completely Fair Scheduler): Explain the red-black tree of virtual runtime (vruntime), the calculation of time slices from task weight and sched_latency, the pick_next_task algorithm, and the sleeper fairness adjustment. Explain the bandwidth controller for CFS (cfs_bandwidth: quota, period, hierarchical enforcement) and how it interacts with container cgroups.
   b. EEVDF (Earliest Eligible Virtual Deadline First): Explain the recent Linux replacement for CFS — the virtual deadline calculation (vd = ve + (request/weight)), the eligibility check (ve ≤ V where V is the server virtual time), and why EEVDF provides better latency guarantees than CFS for interactive workloads. Explain the lag metric and how it prevents starvation.
   c. Real-time scheduling: Explain SCHED_FIFO, SCHED_DEADLINE (CBS: Constant Bandwidth Server with (runtime, deadline, period) parameters), and the admission control test (sum of runtime_i/period_i ≤ total CPU capacity). Explain priority inversion and the priority inheritance protocol (PI mutex). Explain the PREEMPT_RT patch set and its approach to making all spinlocks preemptible.
   d. NUMA-aware scheduling: Explain the NUMA balancing algorithm — periodic page scanning via the NUMA hinting fault mechanism (temporarily unmapping pages to detect access patterns), the automatic NUMA page migration policy, and the task placement heuristics that try to co-locate tasks with their memory. Explain the challenges with memory-intensive workloads where migration overhead exceeds the NUMA locality benefit.

3. File systems and storage stack:
   a. Ext4 journaling: Explain the JBD2 (Journaling Block Device 2) layer — the three journaling modes (journal, ordered, writeback), the transaction lifecycle (running → committing → committed → checkpoint), and the journal recovery procedure. Explain delayed allocation and how it interacts with the journal.
   b. Btrfs copy-on-write: Explain the B-tree structure (metadata trees, extent trees, checksum trees), the COW mechanism for both metadata and data, snapshots as cheap tree clones (sharing all blocks, diverging on write), and the balance/scrub/defrag maintenance operations. Explain the RAID implementation (RAID1/5/6 at the filesystem level, stripe tree for RAID5/6) and the known write hole problem in RAID5.
   c. io_uring: Explain the submission queue (SQ) and completion queue (CQ) ring buffer design, the io_uring_enter syscall, SQPOLL mode for busy-polling without syscalls, and fixed file/buffer registration for zero-copy I/O. Explain the performance comparison with libaio and synchronous I/O for NVMe devices at queue depth 1 and queue depth 128.

4. Compare: (a) 4KB vs huge pages for database workloads (TLB miss rate vs memory waste), (b) CFS vs EEVDF for latency-sensitive containers, (c) ext4 vs btrfs for container overlay filesystems (performance, snapshot cost, stability).

Be precise and technical. Include pseudocode for all key algorithm steps.
PROMPT_EOF

TMPDIR=$(mktemp -d)
trap 'rm -rf "$TMPDIR"' EXIT

# Append a random nonce to each base prompt so re-runs don't hit stale cache
# entries from previous runs. X2 reuses X1's exact prompt within each family.
NONCE_A="session-$(head -c 8 /dev/urandom | od -An -tx1 | tr -d ' \n')"
NONCE_B="session-$(head -c 8 /dev/urandom | od -An -tx1 | tr -d ' \n')"
NONCE_C="session-$(head -c 8 /dev/urandom | od -An -tx1 | tr -d ' \n')"
NONCE_D="session-$(head -c 8 /dev/urandom | od -An -tx1 | tr -d ' \n')"
PROMPT_A_RUN="${PROMPT_A}

[Session nonce: ${NONCE_A}]"
PROMPT_B_RUN="${PROMPT_B}

[Session nonce: ${NONCE_B}]"
PROMPT_C_RUN="${PROMPT_C}

[Session nonce: ${NONCE_C}]"
PROMPT_D_RUN="${PROMPT_D}

[Session nonce: ${NONCE_D}]"

# ── Phase 1: Cold requests (concurrent) ─────────────────────────────
# Send A1, B1, C1, D1 simultaneously so the router sees queue pressure
# and is forced to distribute across workers.
echo
echo "==> Phase 1: Sending cold requests A1, B1, C1, D1 concurrently..."

# Snapshot tokens once before the batch.
for w in $WORKERS; do
  get_prompt_tokens "$w" > "$TMPDIR/batch1.before.$w"
done

fire_request "A1" "$PROMPT_A_RUN" "$TMPDIR" &
fire_request "B1" "$PROMPT_B_RUN" "$TMPDIR" &
fire_request "C1" "$PROMPT_C_RUN" "$TMPDIR" &
fire_request "D1" "$PROMPT_D_RUN" "$TMPDIR" &
wait

# Print latencies.
for lbl in A1 B1 C1 D1; do
  printf "    %s latency: %ss\n" "$lbl" "$(cat "$TMPDIR/$lbl.latency")"
done

# Detect which workers handled the batch by comparing token deltas.
sleep 1
echo
echo "    Worker token deltas from cold batch:"
for w in $WORKERS; do
  before=$(cat "$TMPDIR/batch1.before.$w")
  after=$(get_prompt_tokens "$w")
  delta=$(echo "$after - $before" | bc 2>/dev/null || echo "0")
  printf "      [%s] +%s tokens\n" "$w" "$delta"
  # Save post-batch baseline for the warm phase.
  echo "$after" > "$TMPDIR/batch1.after.$w"
done

# ── Phase 2: Warm requests (sequential) ─────────────────────────────
# Send A2, B2, C2, D2 one at a time. The KV router should send each
# back to the worker that cached the corresponding cold prompt.
# We also use X2's detected pod as X1's pod — the router picks the same
# worker for the same prompt, so the worker that serves X2 (cache hit)
# is the one that served X1 (cold).
echo
echo "==> Phase 2: Sending warm requests sequentially..."
send_request "A2" "$PROMPT_A_RUN" "$TMPDIR"
send_request "B2" "$PROMPT_B_RUN" "$TMPDIR"
send_request "C2" "$PROMPT_C_RUN" "$TMPDIR"
send_request "D2" "$PROMPT_D_RUN" "$TMPDIR"

# Attribute cold requests to the same pod as their warm counterpart.
cp "$TMPDIR/A2.pod" "$TMPDIR/A1.pod"
cp "$TMPDIR/B2.pod" "$TMPDIR/B1.pod"
cp "$TMPDIR/C2.pod" "$TMPDIR/C1.pod"
cp "$TMPDIR/D2.pod" "$TMPDIR/D1.pod"

echo
echo "==> Waiting for worker metrics to flush (up to 20s)..."
# vLLM emits "Prefix cache hit rate" summary lines every ~10s, only when
# there was activity in the window. Poll until at least one appears.
for _ in $(seq 1 20); do
  seen=0
  for w in $WORKERS; do
    if kubectl logs -n "$WORKER_NAMESPACE" "$w" --since-time="$SINCE" 2>/dev/null \
         | grep -q 'Prefix cache hit rate'; then
      seen=1
      break
    fi
  done
  if [[ $seen -eq 1 ]]; then break; fi
  sleep 1
done

A1_LAT=$(cat "$TMPDIR/A1.latency")
A2_LAT=$(cat "$TMPDIR/A2.latency")
B1_LAT=$(cat "$TMPDIR/B1.latency")
B2_LAT=$(cat "$TMPDIR/B2.latency")
C1_LAT=$(cat "$TMPDIR/C1.latency")
C2_LAT=$(cat "$TMPDIR/C2.latency")
D1_LAT=$(cat "$TMPDIR/D1.latency")
D2_LAT=$(cat "$TMPDIR/D2.latency")

# The real signal in agg mode lives in each worker's vLLM engine logs,
# emitted every ~10s as a cumulative summary line:
#   * "Prefix cache hit rate: X%"
#       → fraction of token blocks served from this pod's own on-pod cache
#         instead of recomputing. This is the cross-request reuse signal we
#         want: if A1 populated a pod's cache and the KV-aware router
#         correctly sent A2 back to the SAME pod, that pod's prefix cache
#         hit rate for the window will jump. If the router scattered A1 and
#         A2 across different pods, no pod will show a meaningful hit rate
#         and A2's latency won't drop.
#
# These are cumulative since engine start; we take the LAST line after SINCE.
latest_metric() {
  local pod="$1" pattern="$2"
  kubectl logs -n "$WORKER_NAMESPACE" "$pod" --since-time="$SINCE" 2>/dev/null \
    | grep -oE "$pattern" | tail -1 || true
}

echo
echo "========================================================================"
echo "  KV-aware routing test results"
echo "========================================================================"
printf "  %-4s  %-11s  %s\n" "req" "latency" "pod"
printf "  %-4s  %-11s  %s\n" "A1" "${A1_LAT}s" "$(cat "$TMPDIR/A1.pod")"
printf "  %-4s  %-11s  %s\n" "A2" "${A2_LAT}s" "$(cat "$TMPDIR/A2.pod")"
printf "  %-4s  %-11s  %s\n" "B1" "${B1_LAT}s" "$(cat "$TMPDIR/B1.pod")"
printf "  %-4s  %-11s  %s\n" "B2" "${B2_LAT}s" "$(cat "$TMPDIR/B2.pod")"
printf "  %-4s  %-11s  %s\n" "C1" "${C1_LAT}s" "$(cat "$TMPDIR/C1.pod")"
printf "  %-4s  %-11s  %s\n" "C2" "${C2_LAT}s" "$(cat "$TMPDIR/C2.pod")"
printf "  %-4s  %-11s  %s\n" "D1" "${D1_LAT}s" "$(cat "$TMPDIR/D1.pod")"
printf "  %-4s  %-11s  %s\n" "D2" "${D2_LAT}s" "$(cat "$TMPDIR/D2.pod")"

echo
echo "  Worker prefix cache hit rates since $SINCE:"
for w in $WORKERS; do
  hit=$(latest_metric "$w" 'Prefix cache hit rate: [0-9.]+%')
  printf "    [%s] %s\n" "$w" "${hit:-(no metrics flushed yet)}"
done

echo "========================================================================"
