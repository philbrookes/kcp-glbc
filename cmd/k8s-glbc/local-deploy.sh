#!/bin/bash

#
# Copyright 2021 Red Hat, Inc.
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
#
LOCAL_SETUP_DIR="$(dirname "${BASH_SOURCE[0]}")"
KCP_GLBC_DIR="${LOCAL_SETUP_DIR}/../.."
source "${KCP_GLBC_DIR}"/utils/.setupEnv

export $(cat ${KCP_GLBC_DIR}/config/deploy/local/k8s-glbc/controller-config.env | xargs)
export $(cat ~/.aws-credentials.env | xargs)

#create CRDs
${KUSTOMIZE_BIN} build ${KCP_GLBC_DIR}/config/crd | kubectl apply -f -

#deploy cert manager
kubectl create namespace cert-manager
${KUSTOMIZE_BIN} build ${KCP_GLBC_DIR}/config/deploy/local/cert-manager-k8s --enable-helm --helm-command ${HELM_BIN} | kubectl apply -f -
echo "Waiting for Cert Manager deployments to be ready..."
kubectl -n cert-manager wait --timeout=300s --for=condition=Available deployments --all

# Apply the default glbc-ca issuer
kubectl create namespace kcp-glbc
kubectl apply -n kcp-glbc -f ${KCP_GLBC_DIR}/config/default/issuer.yaml