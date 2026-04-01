// Command ranktable-aggregator (vc-ranktable-aggregator) loads a mounted RankTable
// index, fetches shard ConfigMaps from the API, assembles and validates the payload,
// and writes the decompressed RankTable for the workload. Modes: init (one-shot) or
// sidecar (watch + poll).
package main

import (
	"context"
	"flag"
	"fmt"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2"

	"volcano.sh/volcano/pkg/controllers/ranktable/aggregator"
)

type options struct {
	mode string

	indexFilePath string
	outputPath    string

	kubeConfig string
	masterURL  string

	workers         int
	qps             float64
	maxOriginalSize int64
	pollInterval    time.Duration
	startupJitter   time.Duration
	allowPlainShard bool
	metricsAddr     string
}

func main() {
	rand.Seed(time.Now().UnixNano())
	klog.InitFlags(nil)

	opt := &options{}
	flag.StringVar(&opt.mode, "mode", "sidecar", "run mode: init or sidecar")
	flag.StringVar(&opt.indexFilePath, "index-file-path", "/etc/ranktable/index/index.yaml", "path to mounted ranktable index file")
	flag.StringVar(&opt.outputPath, "output-path", "/etc/ranktable/jobstart_hccl.json", "path to write assembled ranktable file")
	flag.StringVar(&opt.kubeConfig, "kubeconfig", "", "path to kubeconfig; empty means in-cluster config")
	flag.StringVar(&opt.masterURL, "master", "", "kubernetes apiserver address override")
	flag.IntVar(&opt.workers, "workers", 4, "max concurrent shard fetch workers")
	flag.Float64Var(&opt.qps, "kube-api-qps", 3.0, "kube client qps")
	flag.Int64Var(&opt.maxOriginalSize, "max-original-size", 52428800, "max allowed decompressed bytes")
	flag.DurationVar(&opt.pollInterval, "poll-interval", 30*time.Second, "sidecar fallback reconcile interval")
	flag.DurationVar(&opt.startupJitter, "startup-jitter", 30*time.Second, "max startup jitter duration")
	flag.BoolVar(&opt.allowPlainShard, "allow-plain-shard", false, "allow shard ConfigMap values that are not base64 (debug/tests only; not for production)")
	flag.StringVar(&opt.metricsAddr, "metrics-addr", "", "if set (e.g. :9090), listen for Prometheus metrics on /metrics")
	flag.Parse()

	client, err := buildClient(opt.masterURL, opt.kubeConfig, float32(opt.qps), opt.workers*2)
	if err != nil {
		klog.Exitf("build kube client: %v", err)
	}

	reconciler := aggregator.NewReconciler(client, aggregator.Options{
		Workers:         opt.workers,
		RequestQPS:      opt.qps,
		MaxOriginalSize: opt.maxOriginalSize,
		AllowPlainShard: opt.allowPlainShard,
	})

	if opt.metricsAddr != "" {
		mux := http.NewServeMux()
		mux.Handle("/metrics", promhttp.Handler())
		srv := &http.Server{Addr: opt.metricsAddr, Handler: mux}
		go func() {
			klog.InfoS("metrics server listening", "addr", opt.metricsAddr)
			if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				klog.ErrorS(err, "metrics server exited")
			}
		}()
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	switch opt.mode {
	case "init":
		if err := aggregator.RunInit(ctx, reconciler, opt.indexFilePath, opt.outputPath, opt.startupJitter); err != nil {
			klog.Exitf("init reconcile failed: %v", err)
		}
		klog.Info("init reconcile completed")
	case "sidecar":
		err := aggregator.RunSidecar(ctx, reconciler, aggregator.SidecarOptions{
			IndexFilePath: opt.indexFilePath,
			OutputPath:    opt.outputPath,
			PollInterval:  opt.pollInterval,
			StartupJitter: opt.startupJitter,
		})
		if err != nil {
			klog.Exitf("sidecar failed: %v", err)
		}
	default:
		klog.Exitf("invalid --mode: %s (want init|sidecar)", opt.mode)
	}
}

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
