# Azure Cluster Autoscaling for AIRunway

This guide explains how to enable cluster autoscaling for GPU workloads in Azure Kubernetes Service (AKS), allowing your cluster to automatically provision GPU nodes when AI Runway deployments require more resources than currently available.

## Overview

AI Runway integrates with Kubernetes cluster autoscaling to provide visibility and guidance when deploying models that exceed available GPU capacity.

## Prerequisites

- Azure CLI (`az`) installed and authenticated
- `kubectl` configured for your cluster
- Appropriate Azure RBAC permissions (Contributor or higher on cluster/resource group)

---

## Enable Autoscaling on AKS

AKS provides a managed cluster autoscaler that integrates directly with Azure infrastructure.

### Enable Autoscaling on Existing Node Pool

If you already have a GPU node pool, enable autoscaling with:

```bash
# Replace with your actual values
RESOURCE_GROUP="my-resource-group"
CLUSTER_NAME="my-aks-cluster"
NODE_POOL_NAME="gpu"

az aks nodepool update \
  --resource-group $RESOURCE_GROUP \
  --cluster-name $CLUSTER_NAME \
  --name $NODE_POOL_NAME \
  --enable-cluster-autoscaler \
  --min-count 1 \
  --max-count 10
```

### Create New GPU Node Pool with Autoscaling

To create a new GPU node pool with autoscaling enabled:

```bash
az aks nodepool add \
  --resource-group $RESOURCE_GROUP \
  --cluster-name $CLUSTER_NAME \
  --name gpunodepool \
  --node-count 1 \
  --min-count 1 \
  --max-count 10 \
  --node-vm-size Standard_NC24ads_A100_v4 \
  --enable-cluster-autoscaler
```

**Common GPU VM Sizes:**
| VM Size                    | GPUs    | GPU Type | vCPUs | RAM    |
| -------------------------- | ------- | -------- | ----- | ------ |
| `Standard_NC24ads_A100_v4`  | 1x A100 | 80GB     | 24    | 220 GB  |
| `Standard_NC48ads_A100_v4`  | 2x A100 | 160GB    | 48    | 440 GB  |
| `Standard_NC96ads_A100_v4`  | 4x A100 | 320GB    | 96    | 880 GB  |
| `Standard_NC40ads_H100_v5`  | 1x H100 | 80GB     | 40    | 320 GB  |
| `Standard_NC80adis_H100_v5` | 2x H100 | 160GB    | 80    | 640 GB  |
| `Standard_ND96isr_H100_v5`  | 8x H100 | 640GB    | 96    | 1900 GB |

### Update Autoscaler Settings

Adjust min/max node counts:

```bash
az aks nodepool update \
  --resource-group $RESOURCE_GROUP \
  --cluster-name $CLUSTER_NAME \
  --name $NODE_POOL_NAME \
  --update-cluster-autoscaler \
  --min-count 0 \
  --max-count 20
```

**Note:** Setting `--min-count 0` allows scaling down to zero nodes when idle, reducing costs. However, scale-up from zero takes longer (typically 5-10 minutes).

### Disable Autoscaling

To disable autoscaling and maintain a fixed node count:

```bash
az aks nodepool update \
  --resource-group $RESOURCE_GROUP \
  --cluster-name $CLUSTER_NAME \
  --name $NODE_POOL_NAME \
  --disable-cluster-autoscaler
```

---

## Verification

### Check Autoscaler Detection in AIRunway

1. Navigate to **Settings** page in AIRunway
2. Look for **Cluster Autoscaling** section
3. Expected status: **Cluster Autoscaler running on X node group(s)**

### Verify via CLI

```bash
# Check if autoscaler is enabled on node pool
az aks nodepool show \
  --resource-group $RESOURCE_GROUP \
  --cluster-name $CLUSTER_NAME \
  --name $NODE_POOL_NAME \
  --query '{autoscaling: enableAutoScaling, min: minCount, max: maxCount}'
```

### Check Autoscaler Status ConfigMap

AI Runway detects the autoscaler using AKS-specific node labels (`cluster-autoscaler.kubernetes.io/enabled`) first, then falls back to checking the `cluster-autoscaler-status` ConfigMap:

```bash
kubectl get configmap cluster-autoscaler-status -n kube-system -o yaml
```

---

## Troubleshooting

### Issue: AI Runway Shows "Not Detected"

**Check if autoscaling is enabled:**
```bash
az aks nodepool show \
  --resource-group $RESOURCE_GROUP \
  --cluster-name $CLUSTER_NAME \
  --name $NODE_POOL_NAME \
  --query enableAutoScaling
```

If `false`, enable it:
```bash
az aks nodepool update \
  --resource-group $RESOURCE_GROUP \
  --cluster-name $CLUSTER_NAME \
  --name $NODE_POOL_NAME \
  --enable-cluster-autoscaler \
  --min-count 1 \
  --max-count 10
```

### Issue: Pods Stay Pending, No Scale-Up

1. **Check pod scheduling failure reason:**
   ```bash
   kubectl describe pod <pod-name> -n <namespace>
   ```

   Look for events like:
   - ✅ `Insufficient nvidia.com/gpu` → Autoscaler should help
   - ❌ `node(s) didn't match Pod's node affinity` → Configuration issue

2. **Check node pool max capacity:**
   ```bash
   az aks nodepool show \
     --resource-group $RESOURCE_GROUP \
     --cluster-name $CLUSTER_NAME \
     --name $NODE_POOL_NAME \
     --query '{current: count, max: maxCount}'
   ```

   If at max, increase:
   ```bash
   az aks nodepool update ... --max-count 20
   ```

3. **Check Azure quota:**
   ```bash
   az vm list-usage --location eastus --query "[?contains(name.value, 'NC')]" -o table
   ```

   Request increase if needed: https://aka.ms/azure-quota

### Issue: Slow Scale-Up (>10 minutes)

GPU node scale-up typically takes 5-10 minutes. If longer:
- Check Azure Service Health: https://status.azure.com
- GPU VMs may have limited availability in your region

---

## Cost Optimization

### Scale to Zero

Allow GPU nodes to scale to zero when idle:

```bash
az aks nodepool update \
  --resource-group $RESOURCE_GROUP \
  --cluster-name $CLUSTER_NAME \
  --name $NODE_POOL_NAME \
  --update-cluster-autoscaler \
  --min-count 0 \
  --max-count 10
```

**Trade-offs:**
- ✅ Maximum cost savings
- ❌ First deployment takes 5-10 minutes to provision

---

## Reference

- [AKS Cluster Autoscaler](https://learn.microsoft.com/azure/aks/cluster-autoscaler)
- [Azure GPU VM Sizes](https://learn.microsoft.com/azure/virtual-machines/sizes-gpu)
- [Azure Quotas](https://learn.microsoft.com/azure/quotas/quotas-overview)
