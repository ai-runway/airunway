# Headlamp Plugin Local Setup (WSL2 + Windows)

## Prerequisites

- Kind cluster running on Windows (via Docker Desktop)
- Headlamp Desktop installed on Windows
- `kubectl` accessible from WSL with the Windows kubeconfig
- Bun installed in WSL

## 1. Build and Deploy the Plugin

```bash
cd plugins/headlamp
bun install
bun run build
bun run extract

# Deploy to the Windows Headlamp plugins directory
PLUGIN_DIR="/mnt/c/Users/<YOUR_USERNAME>/AppData/Local/Programs/HeadLamp/resources/.plugins/airunway-headlamp-plugin"
mkdir -p "$PLUGIN_DIR"
cp .plugins/* "$PLUGIN_DIR/"
```

Restart Headlamp after deploying.

## 2. Deploy the AI Runway Backend to the Cluster

```bash
export KUBECONFIG="/mnt/c/Users/<YOUR_USERNAME>/.kube/config"

kubectl create namespace airunway-system --dry-run=client -o yaml | kubectl apply -f -
kubectl apply -f deploy/dashboard.yaml
```

Verify the pod is running:

```bash
kubectl -n airunway-system get pods
```

## 3. Port-Forward the Backend Service

This step is required each time after a restart. The Kind cluster network is not directly accessible from Windows, so port-forwarding exposes the backend on `localhost:3001`.

```bash
export KUBECONFIG="/mnt/c/Users/<YOUR_USERNAME>/.kube/config"

kubectl -n airunway-system port-forward svc/airunway 3001:80 --address 0.0.0.0
```

Leave this running in a terminal. The backend will be available at `http://localhost:3001`.

## 4. Configure the Plugin

In Headlamp, go to the AIRunway settings and set:

- **Backend URL:** `http://localhost:3001`

## Troubleshooting

- **Port 3001 already in use:** Kill the existing process with `kill $(lsof -t -i:3001)` and retry.
- **Plugin not appearing:** Verify files exist in the Headlamp `.plugins/airunway-headlamp-plugin/` directory and restart Headlamp.
- **Connection refused in Headlamp:** Ensure the port-forward command is still running.
