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

// Command ranktable-aggregator (vc-ranktable-aggregator) loads a mounted RankTable
// index, fetches shard ConfigMaps from the API, assembles and validates the payload,
// and writes the decompressed RankTable for the workload. Single run loop: bootstrap
// when the output file is missing, then watch + poll for updates.
package main

import (
	"flag"

	"k8s.io/klog/v2"

	"volcano.sh/volcano/cmd/ranktable-aggregator/app"
	"volcano.sh/volcano/cmd/ranktable-aggregator/app/options"
)

func main() {
	klog.InitFlags(nil)

	opt := options.NewServerOption()
	opt.AddFlags(flag.CommandLine)
	flag.Parse()

	if err := app.Run(opt); err != nil {
		klog.Exitf("%v", err)
	}
}
