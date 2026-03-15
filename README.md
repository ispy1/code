# Azure Spot Pod Injector

This project is a Kubernetes Mutating Admission Webhook that automatically injects a toleration for Azure Spot Virtual Machines into newly created Pods. This allows you to schedule pods on Spot node pools without manually adding the toleration to every workload.

The target toleration is: `kubernetes.azure.com/scalesetpriority=spot:NoSchedule`.

## How It Works

The webhook intercepts `CREATE` requests for Pods in the cluster. It checks if the Pod already has the Spot toleration. If not, it adds the toleration via a JSON patch before the Pod is persisted to etcd.

It is designed to be highly available and safe:
- **Failure Policy:** Set to `Ignore` so that a webhook failure will not block pod creation across the cluster.
- **Namespace Safety:** The webhook explicitly ignores critical namespaces: `kube-system`, `cert-manager`, and `gatekeeper-system`.
- **High Availability:** The deployment runs two replicas with Pod Anti-Affinity to ensure they run on different nodes.
- **Node Affinity:** The webhook pods are forced to run on non-Spot nodes to ensure they are always available.

## Prerequisites

1.  A Kubernetes cluster.
2.  `kubectl` installed and configured to connect to your cluster.
3.  **cert-manager** installed in the cluster. This is required for automatic TLS certificate management for the webhook.

### Installing cert-manager

If you do not have `cert-manager` installed, you can install it with the following command. It is recommended to check the [official cert-manager documentation](https://cert-manager.io/docs/installation/) for the latest version.

```sh
kubectl apply -f https://github.com/cert-manager/cert-manager/releases/download/v1.13.2/cert-manager.yaml
```
*(Please verify the version is suitable for your cluster)*

## Installation & Deployment

**Step 1: Build and Push the Docker Image**

You need to build the Docker image and push it to a container registry that your Kubernetes cluster can access (like Docker Hub, ACR, GCR, etc.).

```sh
# Build the image
docker build -t your-repo/spot-injector:latest .

# Push the image
docker push your-repo/spot-injector:latest
```

**Step 2: Update the Manifest**

Open the `k8s-manifests.yaml` file and find the `Deployment` resource. Change the `image` field to the name of the image you just pushed.

```yaml
# in k8s-manifests.yaml
# ...
      containers:
      - name: webhook
        image: your-repo/spot-injector:latest # IMPORTANT: Change this to your image repository
# ...
```

**Step 3: Deploy the Webhook**

Apply the appropriate Kubernetes manifest for your environment.

*   **For Production / Multi-Node Clusters:**
    Use `k8s-manifests.yaml`. This is configured for high availability.
    ```sh
    kubectl apply -f k8s-manifests.yaml
    ```
*   **For Development / Single-Node Clusters:**
    Use `k8s-manifests-dev.yaml`. This is configured with a single replica to work on a one-node cluster.
    ```sh
    kubectl apply -f k8s-manifests-dev.yaml
    ```

### Development / Single-Node Cluster

The production manifest `k8s-manifests.yaml` is configured for high availability with 2 replicas and a `podAntiAffinity` rule to ensure they run on separate nodes. This configuration will fail on a single-node cluster because the second replica can never be scheduled.

For this reason, the `k8s-manifests-dev.yaml` file is provided. It is identical to the production manifest except that it specifies only `1` replica and removes the anti-affinity rule.

## Verification

To verify that the webhook is working correctly, you can create a test pod in a non-excluded namespace (e.g., `default`).

1.  **Create the test pod:**
    ```sh
    kubectl apply -f test-pod.yaml
    ```

2.  **Inspect the pod's tolerations:**
    Check the running pod's YAML to see if the toleration was successfully injected.
    ```sh
    kubectl get pod test-pod -o yaml
    ```

    You should see the following toleration in the pod's `spec`:
    ```yaml
    tolerations:
    - effect: NoSchedule
      key: kubernetes.azure.com/scalesetpriority
      operator: Equal
      value: spot
    # ... other default tolerations
    ```

## Uninstallation

To remove the spot injector and all its associated resources from your cluster, run the `delete` command against the manifest you used for installation.

*   **For Production / Multi-Node Clusters:**
    ```sh
    kubectl delete -f k8s-manifests.yaml
    ```
*   **For Development / Single-Node Clusters:**
    ```sh
    kubectl delete -f k8s-manifests-dev.yaml
    ```
