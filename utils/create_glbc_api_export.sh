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
source "${DEPLOY_SCRIPT_DIR}"/.setupEnv
source "${DEPLOY_SCRIPT_DIR}"/.startUtils

#Workspace
GLBC_WORKSPACE=root:default:kcp-glbc
GLBC_WORKSPACE_USER=root:default:kcp-glbc-user
GLBC_EXPORT_NAME="glbc-${GLBC_WORKSPACE_USER//:/-}"

############################################################
# Help                                                     #
############################################################
help()
{
   # Display Help
   echo "Creates a new GLBC APIExport claiming additional resources from the \"kubernetes\" APIExport used by a given workspace."
   echo
   echo "Syntax: create_glbc_api_export.sh [-n|w|W]"
   echo "options:"
   echo "n     The name of the glbc export to create (default: ${GLBC_EXPORT_NAME})."
   echo "w     The workspace containing the \"kubernetes\" APIBinding being targetted (default: ${GLBC_WORKSPACE_USER})."
   echo "W     The workspace where GLBC is deployed (default: ${GLBC_WORKSPACE})."
   echo
}

############################################################
# print_env                                                #
############################################################
print_env()
{
   echo "Current configuration"
   echo
   echo "KubeConfig:"
   echo
   echo "  KUBECONFIG:                       ${KUBECONFIG}"
   echo
   echo "Workspaces:"
   echo
   echo "  GLBC_EXPORT_NAME:                 ${GLBC_EXPORT_NAME}"
   echo "  GLBC_WORKSPACE_USER:              ${GLBC_WORKSPACE_USER}"
   echo "  GLBC_WORKSPACE:                   ${GLBC_WORKSPACE}"
   echo
}

create_glbc_api_export() {
  name=$1;
  identityHash=$2;
  cat <<EOF | kubectl apply -f -
apiVersion: apis.kcp.dev/v1alpha1
kind: APIExport
metadata:
  name: ${name}
spec:
  latestResourceSchemas:
    - latest.dnsrecords.kuadrant.dev
    - latest.domainverifications.kuadrant.dev
  permissionClaims:
    - group: ""
      resource: "secrets"
    - group: ""
      resource: "services"
      identityHash: ${identityHash}
    - group: "apps"
      resource: "deployments"
      identityHash: ${identityHash}
    - group: "networking.k8s.io"
      resource: "ingresses"
      identityHash: ${identityHash}
EOF
  kubectl wait --timeout=60s --for=condition=VirtualWorkspaceURLsReady=true apiexport $name
}

create_glbc_api_binding() {
  name=$1;
  exportName=$2;
  path=$3;
  identityHash=$4;
  cat <<EOF | kubectl apply -f -
apiVersion: apis.kcp.dev/v1alpha1
kind: APIBinding
metadata:
  name: ${name}
spec:
  reference:
    workspace:
      path: ${path}
      exportName: ${exportName}
  acceptedPermissionClaims:
    - group: ""
      resource: "secrets"
    - group: ""
      resource: "services"
      identityHash: ${identityHash}
    - group: "apps"
      resource: "deployments"
      identityHash: ${identityHash}
    - group: "networking.k8s.io"
      resource: "ingresses"
      identityHash: ${identityHash}
EOF
  kubectl wait --timeout=120s --for=condition=Ready=true apibinding $name
}

############################################################
# Script Start                                             #
############################################################

while getopts "hn:w:W:" arg; do
  case "${arg}" in
    h)
      help
      exit 0
      ;;
    n)
      GLBC_EXPORT_NAME=${OPTARG}
      ;;
    w)
      GLBC_WORKSPACE_USER=${OPTARG}
      ;;
    W)
      GLBC_WORKSPACE=${OPTARG}
      ;;
    *)
      help
      exit 1
      ;;
  esac
done
shift $((OPTIND-1))

set -e pipefail

## Check we are targeting a kcp instance
${KUBECTL_KCP_BIN} workspace . > /dev/null || (echo "You must be targeting a KCP API Server, check your current KUBECONIFG and context before continuing!" && exit 1)

print_env

############################################################
# Check user workspace has appropriate kubernetes config   #
############################################################

${KUBECTL_KCP_BIN} workspace use ${GLBC_WORKSPACE_USER}

# Get the kubernetes APIBinding
kubectl get apibinding kubernetes
kubectl get apibinding kubernetes -o json | jq -r .status.boundResources[0].schema.identityHash

# Check bound resources actually exists, if it doesn't we can assume the export is not ready
kubectl get apibinding kubernetes -o json | jq -e .status.boundResources

# Assumes all required resources are actually there and they all have the same identityHash
# ToDo Check each resource we need actually exists
coreAPIExportIdentityHash=$(kubectl get apibinding kubernetes -o json | jq -r .status.boundResources[0].schema.identityHash)

############################################################
# Create APIExport glbc                                    #
############################################################

${KUBECTL_KCP_BIN} workspace use ${GLBC_WORKSPACE}

## Create glbc APIExport claiming resources from the users kubernetes APIExport
create_glbc_api_export "${GLBC_EXPORT_NAME}" "${coreAPIExportIdentityHash}"

############################################################
# Create APIBinding for glbc and core APIs                 #
############################################################

${KUBECTL_KCP_BIN} workspace use ${GLBC_WORKSPACE_USER}

create_glbc_api_binding "glbc" "${GLBC_EXPORT_NAME}" "${GLBC_WORKSPACE}" "${coreAPIExportIdentityHash}"
