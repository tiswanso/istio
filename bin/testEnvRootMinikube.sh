#!/bin/bash

# Copyright 2018 Istio Authors
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

export K8S_VER=${K8S_VER:-v1.9.2}
export MINIKUBE_VER=${MINIKUBE_VER:-v0.25.0}
set -x

if [ ! -f /usr/local/bin/minikube ]; then
   time curl -Lo minikube "https://storage.googleapis.com/minikube/releases/${MINIKUBE_VER}/minikube-linux-amd64" && chmod +x minikube && sudo mv minikube /usr/local/bin/
fi
if [ ! -f /usr/local/bin/kubectl ]; then
   time curl -Lo kubectl "https://storage.googleapis.com/kubernetes-release/release/${K8S_VER}/bin/linux/amd64/kubectl" && chmod +x kubectl && sudo mv kubectl /usr/local/bin/
fi
if [ ! -f /usr/local/bin/helm ]; then
   curl -Lo helm.tgz https://storage.googleapis.com/kubernetes-helm/helm-v2.8.0-linux-amd64.tar.gz && tar -zxvf helm.tgz && chmod +x linux-amd64/helm && sudo mv linux-amd64/helm /usr/local/bin/
fi


export KUBECONFIG=${KUBECONFIG:-$GOPATH/minikube.conf}

function setupCniBridgePlugin() {
    export CNI_CONF_DIR=${CNI_CONF_DIR:-/etc/cni/net.d}
    export CNI_BIN_DIR=${CNI_BIN_DIR:-/opt/cni/bin}
    export CNI_BIN_RELEASE=${CNI_BIN_RELEASE:-https://github.com/containernetworking/plugins/releases/download/v0.6.0/cni-plugins-amd64-v0.6.0.tgz}
    if [ ! -d ${CNI_CONF_DIR} ]; then
        sudo mkdir -p ${CNI_CONF_DIR}
    fi
    if [ ! -d ${CNI_BIN_DIR} ]; then
        sudo mkdir -p ${CNI_BIN_DIR}
    fi

    sudo chown -R "$(id -u)" ${CNI_CONF_DIR}
    sudo chown -R "$(id -u)" ${CNI_BIN_DIR}
    cat > 10-bridge.conf <<EOF
{
  "name": "kubernetes.io",
  "type": "bridge",
  "bridge": "minikubebr",
  "mtu": 1460,
  "addIf": "true",
  "isGateway": true,
  "ipMasq": true,
  "ipam": {
    "type": "host-local",
    "subnet": "10.1.0.0/16",
    "gateway": "10.1.0.1",
    "routes": [
      {
        "dst": "0.0.0.0/0"
      }
    ]
  }
}
EOF
    sudo mv 10-bridge.conf ${CNI_CONF_DIR}/10-bridge.conf
    # get the standard plugin binaries (bridge, loopback, etc)
    curl -Lo cni-plugins.tgz ${CNI_BIN_RELEASE} && tar -zxvf cni-plugins.tgz -C /opt/cni/bin --strip-components=1 
}

function waitMinikube() {
    set +e
    kubectl cluster-info
    # this for loop waits until kubectl can access the api server that Minikube has created
    for _ in {1..30}; do # timeout for 1 minutes
       kubectl get po --all-namespaces #&> /dev/null
       if [ $? -ne 1 ]; then
          break
      fi
      sleep 2
    done
    if ! kubectl get all --all-namespaces; then
        echo "Kubernetes failed to start"
        ps ax
        netstat -an
        docker images
        cat /var/lib/localkube/localkube.err
        printf '\n\n\n'
        kubectl cluster-info dump
        #exit 1
    fi
    echo "Minikube is running"
}

# Requires sudo ! Start real kubernetes minikube with none driver
function startMinikubeNone() {
    export MINIKUBE_WANTUPDATENOTIFICATION=false
    export MINIKUBE_WANTREPORTERRORPROMPT=false
    export MINIKUBE_HOME=$HOME
    export CHANGE_MINIKUBE_NONE_USER=true

    setupCniBridgePlugin

    sudo -E minikube start \
         --kubernetes-version=v1.9.0 \
         --network-plugin=cni \
         --extra-config=kubelet.network-plugin=cni \
         --vm-driver=none \
         --extra-config=apiserver.Admission.PluginNames="NamespaceLifecycle,LimitRanger,ServiceAccount,DefaultStorageClass,DefaultTolerationSeconds,MutatingAdmissionWebhook,ValidatingAdmissionWebhook,ResourceQuota"
    sudo -E minikube update-context
    sudo chown -R "$(id -u)" "$KUBECONFIG" "$HOME/.minikube"
}

function stopMinikube() {
    sudo minikube stop
}

case "$1" in
    start) startMinikubeNone ;;
    stop) stopMinikube ;;
    wait) waitMinikube ;;
esac
