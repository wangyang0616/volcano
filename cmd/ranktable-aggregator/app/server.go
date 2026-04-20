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

package app

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2"

	"volcano.sh/volcano/cmd/ranktable-aggregator/app/options"
	"volcano.sh/volcano/pkg/controllers/ranktable/aggregator"
)

// Run initializes dependencies and starts the long-running aggregation loop.
func Run(opt *options.ServerOption) error {
	client, err := buildClient(opt.MasterURL, opt.KubeConfig, float32(opt.QPS), opt.Workers*kubeBurstMultiplier)
	if err != nil {
		return fmt.Errorf("build kube client: %w", err)
	}

	reconciler := aggregator.NewReconciler(client, aggregator.Options{
		Workers:         opt.Workers,
		RequestQPS:      opt.QPS,
		MaxOriginalSize: opt.MaxOriginalSize,
		AllowPlainShard: opt.AllowPlainShard,
	})

	if opt.MetricsAddr != "" {
		mux := http.NewServeMux()
		mux.Handle("/metrics", promhttp.Handler())
		srv := &http.Server{Addr: opt.MetricsAddr, Handler: mux}
		go func() {
			klog.InfoS("metrics server listening", "addr", opt.MetricsAddr)
			if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				klog.ErrorS(err, "metrics server exited")
			}
		}()
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if err := aggregator.Run(ctx, reconciler, aggregator.RunOptions{
		IndexFilePath: opt.IndexFilePath,
		OutputPath:    opt.OutputPath,
		PollInterval:  opt.PollInterval,
		StartupJitter: opt.StartupJitter,
	}); err != nil {
		return fmt.Errorf("ranktable aggregator exited: %w", err)
	}
	return nil
}

const kubeBurstMultiplier = 2

// buildClient uses kubeconfig/master or in-cluster config and sets REST QPS/burst.
func buildClient(master, kubeconfig string, qps float32, burst int) (kubernetes.Interface, error) {
	var cfg *rest.Config
	var err error
	if kubeconfig != "" || master != "" {
		cfg, err = clientcmd.BuildConfigFromFlags(master, kubeconfig)
	} else {
		cfg, err = rest.InClusterConfig()
	}
	if err != nil {
		return nil, fmt.Errorf("build rest config: %w", err)
	}
	cfg.QPS = qps
	cfg.Burst = burst
	return kubernetes.NewForConfig(cfg)
}
