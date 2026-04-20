/*
Copyright 2026 The Volcano Authors.

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

package options

import (
	"flag"
	"time"

	"volcano.sh/volcano/pkg/controllers/ranktable/aggregator"
)

const (
	defaultWorkers       = 4
	defaultKubeAPIQPS    = 3.0
	defaultPollInterval  = 30 * time.Second
	defaultStartupJitter = 30 * time.Second
)

// ServerOption defines startup parameters for ranktable-aggregator.
type ServerOption struct {
	IndexFilePath string
	OutputPath    string

	KubeConfig string
	MasterURL  string

	Workers         int
	QPS             float64
	MaxOriginalSize int64
	PollInterval    time.Duration
	StartupJitter   time.Duration
	AllowPlainShard bool
	MetricsAddr     string
}

// NewServerOption returns a zero-value option object.
func NewServerOption() *ServerOption {
	return &ServerOption{}
}

// AddFlags binds ranktable-aggregator flags to this option set.
func (s *ServerOption) AddFlags(fs *flag.FlagSet) {
	fs.StringVar(&s.IndexFilePath, "index-file-path", "/etc/ranktable/index/index.yaml", "path to mounted ranktable index file")
	fs.StringVar(&s.OutputPath, "output-path", "/etc/ranktable/jobstart_hccl.json", "path to write assembled ranktable file")
	fs.StringVar(&s.KubeConfig, "kubeconfig", "", "path to kubeconfig; empty means in-cluster config")
	fs.StringVar(&s.MasterURL, "master", "", "kubernetes apiserver address override")
	fs.IntVar(&s.Workers, "workers", defaultWorkers, "max concurrent shard fetch workers")
	fs.Float64Var(&s.QPS, "kube-api-qps", defaultKubeAPIQPS, "kube client qps")
	fs.Int64Var(&s.MaxOriginalSize, "max-original-size", aggregator.DefaultMaxOriginalSize, "max allowed decompressed bytes")
	fs.DurationVar(&s.PollInterval, "poll-interval", defaultPollInterval, "periodic reconcile interval (fallback if fsnotify misses)")
	fs.DurationVar(&s.StartupJitter, "startup-jitter", defaultStartupJitter, "max startup jitter duration")
	fs.BoolVar(&s.AllowPlainShard, "allow-plain-shard", false, "allow shard ConfigMap values that are not base64 (debug/tests only; not for production)")
	fs.StringVar(&s.MetricsAddr, "metrics-addr", "", "if set (e.g. :9090), listen for Prometheus metrics on /metrics")
}
