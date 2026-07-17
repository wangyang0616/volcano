#!/bin/bash

# Copyright 2019 The Volcano Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# spin up cluster with kind command
function kind-up-cluster {
  check-kind

  echo "Running kind: [kind create cluster ${CLUSTER_CONTEXT[*]} ${KIND_OPT}]"
  kind create cluster "${CLUSTER_CONTEXT[@]}" ${KIND_OPT}

  echo
  check-images

  echo
  echo "Loading docker images into kind cluster"
  # only need to load images into control-plane node because volcano components are deployed on control-plane node.
  kind load docker-image ${IMAGE_PREFIX}/vc-controller-manager:${TAG} "${CLUSTER_CONTEXT[@]}" --nodes ${CLUSTER_CONTEXT[1]}-control-plane
  kind load docker-image ${IMAGE_PREFIX}/vc-scheduler:${TAG}          "${CLUSTER_CONTEXT[@]}" --nodes ${CLUSTER_CONTEXT[1]}-control-plane
  kind load docker-image ${IMAGE_PREFIX}/vc-webhook-manager:${TAG}    "${CLUSTER_CONTEXT[@]}" --nodes ${CLUSTER_CONTEXT[1]}-control-plane
  if [[ "${E2E_TYPE}" == "REPACK" ]]; then
    kind load docker-image ${IMAGE_PREFIX}/vc-repack-engine:${TAG} "${CLUSTER_CONTEXT[@]}" --nodes ${CLUSTER_CONTEXT[1]}-control-plane
    ensure-repack-test-images
  fi
}

function ensure-repack-test-images {
  local repack_images=(
    "nginx:1.29.3-alpine"
  )

  echo
  echo "Ensuring repack test images are available locally"
  for image in "${repack_images[@]}"; do
    if ! docker image inspect "${image}" >/dev/null 2>&1; then
      echo "Pulling image ${image} ..."
      docker pull "${image}" >/dev/null || exit 1
    fi
  done

  echo
  echo "Loading repack test images into kind cluster"
  for image in "${repack_images[@]}"; do
    kind load docker-image "${image}" "${CLUSTER_CONTEXT[@]}" || exit 1
  done
}

# check if the required images exist
function check-images {
  echo "Checking whether the required images exist"
  docker image inspect "${IMAGE_PREFIX}/vc-controller-manager:${TAG}" > /dev/null
  if [[ $? -ne 0 ]]; then
    echo -e "\033[31mERROR\033[0m: ${IMAGE_PREFIX}/vc-controller-manager:${TAG} does not exist"
    exit 1
  fi
  docker image inspect "${IMAGE_PREFIX}/vc-scheduler:${TAG}" > /dev/null
  if [[ $? -ne 0 ]]; then
    echo -e "\033[31mERROR\033[0m: ${IMAGE_PREFIX}/vc-scheduler:${TAG} does not exist"
    exit 1
  fi
  docker image inspect "${IMAGE_PREFIX}/vc-webhook-manager:${TAG}" > /dev/null
  if [[ $? -ne 0 ]]; then
    echo -e "\033[31mERROR\033[0m: ${IMAGE_PREFIX}/vc-webhook-manager:${TAG} does not exist"
    exit 1
  fi
  if [[ "${E2E_TYPE}" == "REPACK" ]]; then
    docker image inspect "${IMAGE_PREFIX}/vc-repack-engine:${TAG}" > /dev/null
    if [[ $? -ne 0 ]]; then
      echo -e "\033[31mERROR\033[0m: ${IMAGE_PREFIX}/vc-repack-engine:${TAG} does not exist"
      exit 1
    fi
  fi
}

# check if kubectl installed
function check-prerequisites {
  echo "Checking prerequisites"
  which kubectl >/dev/null 2>&1
  if [[ $? -ne 0 ]]; then
    echo -e "\033[31mERROR\033[0m: kubectl not installed"
    exit 1
  else
    echo -n "Found kubectl, version: " && kubectl version --client
  fi
}

# check if kind installed
function check-kind {
  echo "Checking kind"
  local required_kind_version="0.31.0"
  local bin_path
  bin_path=$(go env GOBIN)
  if [[ -z "${bin_path}" ]]; then
    bin_path="$(go env GOPATH)/bin"
  fi
  export PATH="${bin_path}:${PATH}"

  which kind >/dev/null 2>&1
  if [[ $? -ne 0 ]]; then
    echo "Installing kind ${required_kind_version} ..."
    GOOS=${OS} go install sigs.k8s.io/kind@v${required_kind_version}
    if ! command -v kind >/dev/null 2>&1; then
      echo -e "\033[31mERROR\033[0m: kind installation completed but the binary is still not available on PATH"
      exit 1
    fi
    echo -n "Using kind, version: " && kind version
    return
  fi

  local found_version
  found_version=$(kind version 2>/dev/null | awk '{print $2}' | tr -d 'v')
  echo -n "Found kind, version: " && kind version
  if [[ -z "${found_version}" ]]; then
    echo -e "\033[33mWARNING\033[0m: unable to parse kind version; expected v${required_kind_version}+ for Repack E2E"
    return
  fi

  # Repack E2E uses a recent Kubernetes node image and a multi-worker Kind
  # configuration. Keep Kind new enough to support that cluster definition.
  if [[ "${found_version}" < "${required_kind_version}" ]]; then
    echo -e "\033[33mWARNING\033[0m: kind v${found_version} is older than v${required_kind_version}; upgrading..."
    GOOS=${OS} go install sigs.k8s.io/kind@v${required_kind_version}
    export PATH="${bin_path}:${PATH}"
    echo -n "Using kind, version: " && kind version
  fi
}

# install helm if not installed
function install-helm {
  echo "Checking helm"
  which helm >/dev/null 2>&1
  if [[ $? -ne 0 ]]; then
    echo "Installing helm via script"
    HELM_TEMP_DIR=$(mktemp -d)
    curl -fsSL -o ${HELM_TEMP_DIR}/get_helm.sh https://raw.githubusercontent.com/helm/helm/main/scripts/get-helm-3
    chmod 700 ${HELM_TEMP_DIR}/get_helm.sh && ${HELM_TEMP_DIR}/get_helm.sh
  else
    echo -n "Found helm, version: " && helm version
  fi
}

function install-ginkgo-if-not-exist {
  echo "Checking ginkgo"
  which ginkgo >/dev/null 2>&1
  if [[ $? -ne 0 ]]; then
    echo "Installing ginkgo ..."
    GOOS=${OS} go install github.com/onsi/ginkgo/v2/ginkgo
  else
    echo -n "Found ginkgo, version: " && ginkgo version
  fi
}

function install-kwok-with-helm {
  helm repo add kwok https://kwok.sigs.k8s.io/charts/
  helm repo update
  helm upgrade --namespace kube-system --install kwok kwok/kwok
  helm upgrade --install kwok kwok/stage-fast
  # delete pod-complete stage to avoid volcano-job-pod change status to complete.
  kubectl delete stage pod-complete
}
