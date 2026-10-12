const MAX_STORAGE_SAFE_DEPLOYMENT_NAME_LENGTH = 48;

export function generateDeploymentName(modelId: string): string {
  return modelId
    .toLowerCase()
    .replace(/[^a-z0-9-]+/g, '-')
    .replace(/--+/g, '-')
    .replace(/^-|-$/g, '')
    .slice(0, MAX_STORAGE_SAFE_DEPLOYMENT_NAME_LENGTH)
    .replace(/-+$/g, '');
}
