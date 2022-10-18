#!/bin/bash

#
# Copyright 2022 Red Hat, Inc.
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

DEPLOY_SCRIPT_DIR="$(dirname "${BASH_SOURCE[0]}")"
KCP_GLBC_DIR="${DEPLOY_SCRIPT_DIR}/.."
source "${DEPLOY_SCRIPT_DIR}"/.setupEnv
source "${DEPLOY_SCRIPT_DIR}"/.startUtils

#Workspace
GLBC_WORKSPACE=root:kuadrant

#Syn Targets
: ${GLBC_SYNC_TARGET_NAME:=kcp-cluster-1}

: ${KCP_VERSION:="release-0.9"}
KCP_SYNCER_IMAGE="ghcr.io/kcp-dev/kcp/syncer:${KCP_VERSION}"

# GLBC Deployment
GLBC_NAMESPACE=kcp-glbc
DEPLOY_COMPONENTS=glbc,cert-manager
KUSTOMIZATION_DIR=${KCP_GLBC_DIR}/config/deploy/local

############################################################
# Help                                                     #
############################################################
help()
{
  # Display Help
  echo "Prepares a KCP workspace and deploys GLBC and its dependant components into it."
  echo
  echo "Syntax: deploy.sh [-c|k|m|n|h|w|s]"
  echo "options:"
  echo "c     Components to deploy (default: ${DEPLOY_COMPONENTS})"
  echo "k     GLBC deployment kustomization directory, must contain a kcp-glbc and cert-manager sub directory (default: ${KUSTOMIZATION_DIR})"
  echo "n     Namespace glbc is being deployed into (default: ${GLBC_NAMESPACE})"
  echo "h     Print this Help."
  echo "w     Workspace to deploy glbc into (default: ${GLBC_WORKSPACE})."
  echo "s     synctarget name"
  echo
}

############################################################
# print_env                                                #
############################################################
print_env()
{
   echo "Current deployment configuration"
   echo
   echo "KubeConfig:"
   echo
   echo "  KUBECONFIG:                       ${KUBECONFIG}"
   echo
   echo "Workspaces:"
   echo
   echo "  GLBC_WORKSPACE:                   ${GLBC_WORKSPACE}"
   echo "  GLBC_EXPORT_NAME:                 ${GLBC_EXPORT_NAME}"
   echo
   echo "Sync Targets:"
   echo
   echo "  GLBC_SYNC_TARGET_NAME:            ${GLBC_SYNC_TARGET_NAME}"
   echo "  KCP_SYNCER_IMAGE:                 ${KCP_SYNCER_IMAGE}"
   echo
   echo "GLBC Deployment:"
   echo
   echo "  GLBC_NAMESPACE:                   ${GLBC_NAMESPACE}"
   echo "  DEPLOY_COMPONENTS:                ${DEPLOY_COMPONENTS}"
   echo "  KUSTOMIZATION_DIR:                ${KUSTOMIZATION_DIR}"
   echo "  KCP_GLBC_KUSTOMIZATION_DIR:       ${KCP_GLBC_KUSTOMIZATION_DIR}"
   echo "  CERT_MANAGER_KUSTOMIZATION_DIR:   ${CERT_MANAGER_KUSTOMIZATION_DIR}"
   echo "  SYNC_TARGETS_DIR:                 ${SYNC_TARGETS_DIR}"
   echo
   echo "Misc:"
   echo
   echo "  WAIT_WC_READY                     ${WAIT_WC_READY}"
   echo "  KUBECTL_KCP_BIN                   ${KUBECTL_KCP_BIN}"
   echo
}

create_ns() {
  echo "Creating namespace '${1}'"
  kubectl create namespace ${1} --dry-run=client -o yaml | kubectl apply -f -
}

create_sync_target() {
  kubectl get synctargets ${GLBC_SYNC_TARGET_NAME} || {
    echo "Creating workload cluster '${1}'"
    ${KUBECTL_KCP_BIN} workload sync ${1} --kcp-namespace kcp-syncer --syncer-image=${KCP_SYNCER_IMAGE} --resources=ingresses.networking.k8s.io,services --output-file ${SYNC_TARGETS_DIR}/${1}-syncer.yaml
    echo "Apply the following syncer config to the intended physical cluster."
    echo ""
    echo "   kubectl apply -f ${SYNC_TARGETS_DIR}/${1}-syncer.yaml"
    echo ""
  }
  kubectl annotate --overwrite synctarget ${1} featuregates.experimental.workload.kcp.dev/advancedscheduling='true'
  kubectl label --overwrite synctarget ${1} kuadrant.dev/synctarget=${1}
  kubectl wait --timeout=60s --for=condition=VirtualWorkspaceURLsReady=true apiexport kubernetes

  if [[ $WAIT_WC_READY = "true" ]]; then
    echo "This script will automatically continue once the cluster is synced!"
    echo "Waiting for workload cluster ${1} to be ready ..."
    kubectl wait --timeout=300s --for=condition=Ready=true synctargets ${1}
  fi
}

deploy_cert_manager() {
  echo "Deploying Cert Manager"
  create_ns "cert-manager"
  ${KUSTOMIZE_BIN} build ${CERT_MANAGER_KUSTOMIZATION_DIR} --enable-helm --helm-command ${HELM_BIN} | kubectl apply -f -
  echo "Waiting for Cert Manager deployments to be ready..."
  #When advancedscheduling is enabled the status check on deployments never works
  #kubectl -n cert-manager wait --timeout=300s --for=condition=Available deployments --all
}

deploy_glbc() {
  echo "Creating GLBC namespace"
  create_ns ${GLBC_NAMESPACE}

  echo "Deploying GLBC"
  ${KUSTOMIZE_BIN} build ${KCP_GLBC_KUSTOMIZATION_DIR} | kubectl apply -f -
  echo "Waiting for GLBC deployments to be ready..."
  #When advancedscheduling is enabled the status check on deployments never works
  #kubectl -n ${GLBC_NAMESPACE} wait --timeout=300s --for=condition=Available deployments --all
}

############################################################
# Script Start                                             #
############################################################

while getopts "c:k:n:hw:W:s:" arg; do
  case "${arg}" in
    c)
      DEPLOY_COMPONENTS=${OPTARG}
      ;;
    k)
      KUSTOMIZATION_DIR=${OPTARG}
      ;;
    n)
      GLBC_NAMESPACE=${OPTARG}
      ;;
    h)
      help
      exit 0
      ;;
    w)
      GLBC_WORKSPACE=${OPTARG}
      ;;
    s)
      GLBC_SYNC_TARGET_NAME=${OPTARG}
      ;;
    *)
      help
      exit 1
      ;;
  esac
done
shift $((OPTIND-1))

#Workspace
: ${GLBC_EXPORT_NAME:="glbc-${GLBC_WORKSPACE//:/-}"}

# Misc
# Wait for sync targets to be ready before continuing
: ${WAIT_WC_READY:="false"}
: ${KCP_GLBC_KUSTOMIZATION_DIR:=${KUSTOMIZATION_DIR}/kcp-glbc}
: ${CERT_MANAGER_KUSTOMIZATION_DIR:=${KUSTOMIZATION_DIR}/cert-manager}
: ${SYNC_TARGETS_DIR:=${KUSTOMIZATION_DIR}/../sync-targets}

set -e pipefail

## Check we are targeting a kcp instance
${KUBECTL_KCP_BIN} workspace . > /dev/null || (echo "You must be targeting a KCP API Server, check your current KUBECONIFG and context before continuing!" && exit 1)

print_env
echo "Continuing in 10 seconds, Ctrl+C to stop ..."
sleep 10

${KUBECTL_KCP_BIN} workspace use ${GLBC_WORKSPACE}

############################################################
# Create glbc sync target                                  #
############################################################

## Create GLBC Sync Target
kubectl create namespace kcp-syncer --dry-run=client -o yaml | kubectl apply -f -
create_sync_target ${GLBC_SYNC_TARGET_NAME}

############################################################
# Register APIs                                            #
############################################################

## Bind to compute APIs
${KUSTOMIZE_BIN} build ${KCP_GLBC_KUSTOMIZATION_DIR}/apiexports/kubernetes | kubectl apply -f -

## Register GLBC APIs with KCP
${KUSTOMIZE_BIN} build ${KCP_GLBC_KUSTOMIZATION_DIR}/kcp-contrib | kubectl apply -f -

## Register CertManager APIs with KCP
${KUSTOMIZE_BIN} build --reorder none ${CERT_MANAGER_KUSTOMIZATION_DIR}/kcp-contrib | kubectl apply -f -

## Stop here if DEPLOY_COMPONENTS=none (The sync targets are likely not ready)
if [[ $DEPLOY_COMPONENTS =~ "none" ]]; then
  echo "Finished deploying resources. Not deploying any components as DEPLOY_COMPONENTS=${DEPLOY_COMPONENTS}."
  exit 0
fi

############################################################
# Setup GLBC APIExport                                     #
############################################################

if OUTPUT_DIR=${KCP_GLBC_KUSTOMIZATION_DIR}/apiexports/glbc ${DEPLOY_SCRIPT_DIR}/create_glbc_api_export.sh -w "${GLBC_WORKSPACE}" -W "${GLBC_WORKSPACE}" -n "${GLBC_EXPORT_NAME}"; then
  echo "GLBC APIExport created successfully for ${GLBC_WORKSPACE} workspace!"
else
  echo "GLBC APIExport could not be created!"
  # If the GLBC APIExport can't be created, we shouldn't continue to try and deploy anything!
  exit 1
fi

## Apply GLBC APIExport
APIEXPORT_DIR=${KCP_GLBC_KUSTOMIZATION_DIR}/apiexports
${KUSTOMIZE_BIN} build ${APIEXPORT_DIR} | kubectl apply -f -
kubectl wait --timeout=120s --for=condition=Ready=true apibinding "glbc"
kubectl wait --timeout=60s --for=condition=VirtualWorkspaceURLsReady=true apiexport ${GLBC_EXPORT_NAME}

## Dump the APIBinding to a file for use in user workspaces locally
kubectl apply view-last-applied apibinding glbc -o yaml > ${APIEXPORT_DIR}/glbc/glbc-apibinding.yaml

############################################################
# Deploy GLBC Components                                   #
############################################################

## Deploy components
if [[ $DEPLOY_COMPONENTS =~ "cert-manager" ]]; then
  deploy_cert_manager
fi

if [[ $DEPLOY_COMPONENTS =~ "glbc" ]]; then
  deploy_glbc

  echo ""
  echo "GLBC is now running."
  echo ""
  echo "Try deploying the sample service:"
  echo ""
  echo "     ${KUBECTL_KCP_BIN} ws ${GLBC_WORKSPACE}"
  echo "     kubectl apply -f samples/echo-service/echo.yaml"
  echo ""
fi
