# Copyright 2026 The Volcano Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#    http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# Builds the STANDALONE repack-controller from its own module
# (staging/src/volcano.sh/repack-controller). Used only for the standalone
# deployment path; the default path compiles the controller into
# volcano-controller-manager instead.
FROM golang:1.25.0 AS builder
WORKDIR /go/src/volcano.sh/
ADD . volcano
RUN cd volcano && make vc-repack-controller

FROM alpine:latest
COPY --from=builder /go/src/volcano.sh/volcano/_output/bin/vc-repack-controller /vc-repack-controller
ENTRYPOINT ["/vc-repack-controller"]
