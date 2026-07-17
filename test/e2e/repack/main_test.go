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

package repack

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	vcclient "volcano.sh/apis/pkg/client/clientset/versioned"

	e2eutil "volcano.sh/volcano/test/e2e/util"
)

func TestMain(m *testing.M) {
	// Ginkgo dry-run is a static suite inspection and must work on developer
	// machines without a live kubeconfig. BeforeEach bodies are not executed.
	if ginkgoDryRunRequested(os.Args[1:]) {
		os.Exit(m.Run())
	}
	home := e2eutil.HomeDir()
	configPath := e2eutil.KubeconfigPath(home)
	config, err := clientcmd.BuildConfigFromFlags(e2eutil.MasterURL(), configPath)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "build Repack E2E Kubernetes config: %v\n", err)
		os.Exit(1)
	}
	e2eutil.VcClient = vcclient.NewForConfigOrDie(config)
	e2eutil.KubeClient = kubernetes.NewForConfigOrDie(config)
	os.Exit(m.Run())
}

func ginkgoDryRunRequested(arguments []string) bool {
	for _, argument := range arguments {
		normalized := strings.TrimLeft(argument, "-")
		if normalized == "ginkgo.dry-run" || normalized == "ginkgo.dry-run=true" {
			return true
		}
	}
	return false
}
