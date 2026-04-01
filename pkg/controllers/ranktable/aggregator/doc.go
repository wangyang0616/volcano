// Package aggregator loads a RankTable index (from a mounted ConfigMap or raw YAML),
// fetches shard ConfigMaps through the Kubernetes API, concatenates and validates the
// compressed stream, decompresses to the original RankTable bytes, and writes the
// output file atomically. Rate limiting and optional shard reuse (prev_version /
// changed_shards) reduce apiserver load at scale.
//
// Design, protocol, and usage: README.md in this directory.
// Long-form design: ../../../../docs/design/ranktable-sharded-distribution.md
package aggregator
