#!/usr/bin/env bash
set -aeuo pipefail

echo "Running setup.sh"

###
# Cleanup previous run
###
echo "Deleting statefulset and service from previous run if present..."
${KUBECTL} delete statefulset k3s-nodepool -n crossplane-system --ignore-not-found
${KUBECTL} delete service k3s-nodepool -n crossplane-system --ignore-not-found

###
# Image build
###
echo "Building node image and loading it into kind..."
cat <<EOF | docker build -t provider-k3s-node -
FROM ghcr.io/hifis-net/ubuntu-systemd:24.04
RUN apt-get update \
    && apt-get install -y --no-install-recommends openssh-server sudo curl \
    && rm -rf /var/lib/apt/lists/* \
    && useradd -m -s /bin/bash k3s \
    && echo 'k3s ALL=(ALL) NOPASSWD:ALL' > /etc/sudoers.d/k3s \
    && printf 'AuthorizedKeysFile /etc/ssh/authorized_keys.d/%%u\nStrictModes no\n' > /etc/ssh/sshd_config.d/authorized_keys.conf \
    && systemctl disable ssh.socket \
    && systemctl enable ssh.service

CMD ["/lib/systemd/systemd"]
EOF

${KIND} load docker-image provider-k3s-node --name "${KIND_CLUSTER_NAME:-local-dev}"

###
# SSH configuration
###
echo "Creating SSH keys..."

KEY_DIR="$(mktemp -d)"
trap 'rm -rf "${KEY_DIR}"' EXIT

ssh-keygen -q -t ed25519 -N "" -C "provider-k3s-e2e" -f "${KEY_DIR}/id_ed25519"

${KUBECTL} -n ${CROSSPLANE_NAMESPACE} create secret generic e2e-ssh-credentials \
  --from-file=ssh-privatekey="${KEY_DIR}/id_ed25519" \
  --dry-run=client -o yaml | ${KUBECTL} apply -f -

# ConfigMap key must match the ProviderConfig username (sshd_config's
# AuthorizedKeysFile is set to /etc/ssh/authorized_keys.d/%u).
${KUBECTL} -n ${CROSSPLANE_NAMESPACE} create configmap e2e-ssh-authorized-keys \
  --from-file=k3s="${KEY_DIR}/id_ed25519.pub" \
  --dry-run=client -o yaml | ${KUBECTL} apply -f -

###
# Statefulset deployment
###
echo "Creating the nodepool statefulset..."

cat <<EOF | ${KUBECTL} apply -f -
  apiVersion: v1
  kind: Service
  metadata:
    name: k3s-nodepool
    namespace: ${CROSSPLANE_NAMESPACE}
  spec:
    clusterIP: None
    selector:
      app.kubernetes.io/name: k3s-nodepool
    ports:
      - {name: ssh, port: 22, targetPort: ssh}
      - {name: api, port: 6443, targetPort: api}
      - {name: kubelet, port: 10250, targetPort: kubelet}
---
apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: k3s-nodepool
  namespace: ${CROSSPLANE_NAMESPACE}
  labels:
    app.kubernetes.io/name: k3s-nodepool
    app.kubernetes.io/part-of: k3s-nodepool
spec:
  serviceName: k3s-nodepool
  replicas: 2
  selector:
    matchLabels:
      app.kubernetes.io/name: k3s-nodepool
  template:
    metadata:
      labels:
        app.kubernetes.io/name: k3s-nodepool
    spec:
      containers:
        - name: node
          image: provider-k3s-node
          imagePullPolicy: IfNotPresent
          securityContext:
            privileged: true
          ports:
            - name: ssh
              containerPort: 22
            - name: api
              containerPort: 6443
            - name: kubelet
              containerPort: 10250
          volumeMounts:
            - name: authorized-keys
              mountPath: /etc/ssh/authorized_keys.d
              readOnly: true
            - name: tmp
              mountPath: /tmp
            - name: run
              mountPath: /run
            - name: data
              mountPath: /var/lib/rancher
      volumes:
        - name: authorized-keys
          configMap:
            name: e2e-ssh-authorized-keys
            defaultMode: 0644
        - name: tmp
          emptyDir:
            medium: Memory
        - name: run
          emptyDir:
            medium: Memory
  volumeClaimTemplates:
    - metadata:
        name: data
      spec:
        accessModes: ["ReadWriteOnce"]
        storageClassName: standard
        resources:
          requests:
            storage: 1Gi
EOF
kubectl rollout status statefulset/k3s-nodepool --timeout=300s -n ${CROSSPLANE_NAMESPACE}

###
# Provider configuration
###
echo "Creating the provider config with nodepool statefulset ssh keys..."

cat <<EOF | ${KUBECTL} apply -f -
apiVersion: k3s.m.crossplane.io/v1alpha1
kind: ClusterProviderConfig
metadata:
  name: default
spec:
  username: k3s
  credentials:
    source: Secret
    secretRef:
      namespace: ${CROSSPLANE_NAMESPACE}
      name: e2e-ssh-credentials
      key: ssh-privatekey
EOF

cat <<EOF | ${KUBECTL} apply -f -
apiVersion: k3s.crossplane.io/v1alpha1
kind: ProviderConfig
metadata:
  name: default
spec:
  username: k3s
  credentials:
    source: Secret
    secretRef:
      namespace: ${CROSSPLANE_NAMESPACE}
      name: e2e-ssh-credentials
      key: ssh-privatekey
EOF
