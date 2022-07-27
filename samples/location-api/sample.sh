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

export KUBECONFIG=./.kcp/admin.kubeconfig
BASE_WORKSPACE=root:default
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

kubectl kcp workspace ${BASE_WORKSPACE}:kcp-glbc-user-compute

echo "creating locations for sync targets in compute workspace"
kubectl apply -f ${SCRIPT_DIR}/locations.yaml

echo "creating placement in user workspace"
kubectl kcp workspace ${BASE_WORKSPACE}:kcp-glbc-user
kubectl apply -f ${SCRIPT_DIR}/placement-1.yaml
kubectl delete placement default

echo "deploying workload resources in user workspace"
kubectl apply -f ${SCRIPT_DIR}/../echo-service/echo.yaml

echo
echo "=== useful commands:"
echo "  - watch -n1 \"curl -k https://"$(kubectl get ingress ingress-nondomain -o json | jq ".metadata.annotations[\"kuadrant.dev/host.generated\"]" -r)"\" (N.B. Don't start this before A record exists in route53)"
echo "  - watch -n1 'kubectl get dnsrecords ingress-nondomain -o yaml | yq eval \".spec\" -'"
echo "  - watch -n1 -d 'kubectl get ingress ingress-nondomain -o yaml | yq eval \".metadata\" - | grep -v \"kubernetes.io\"'"
echo
echo

read -p "Press enter to trigger migration from kcp-cluster-1 to kcp-cluster-2"

echo "creating placement 2 in user workspace"
kubectl kcp workspace ${BASE_WORKSPACE}:kcp-glbc-user
kubectl create -f ${SCRIPT_DIR}/placement-2.yaml
kubectl delete placement placement-1

read -p "Press enter to reset cluster"
kubectl delete -f ${SCRIPT_DIR}/../echo-service/echo.yaml

echo "resetting placement"
kubectl apply -f ${SCRIPT_DIR}/reset-placement.yaml
kubectl delete placement placement-2

echo "deleting locations"
kubectl kcp workspace ${BASE_WORKSPACE}:kcp-glbc-user-compute

echo "deleting locations for sync targets in compute workspace"
kubectl delete -f ${SCRIPT_DIR}/locations.yaml

kubectl kcp workspace ${BASE_WORKSPACE}:kcp-glbc-user