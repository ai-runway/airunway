import type { AikitBuildRequest, AikitBuildResult, AikitInfrastructureStatus } from '@/lib/api'

import type { GgufRunMode, RuntimeId } from './deploymentFormModel'

type DeploymentToastVariant = 'success' | 'destructive'

interface DeploymentBuildNotification {
  title: string
  description?: string
  variant?: DeploymentToastVariant
}

export interface GgufImageBuildAdapter {
  notify(notification: DeploymentBuildNotification): void
  getInfrastructureStatus(): Promise<AikitInfrastructureStatus>
  build(request: AikitBuildRequest): Promise<AikitBuildResult>
}

export interface PrepareGgufImageRefOptions {
  selectedRuntime: RuntimeId
  isHuggingFaceGgufModel: boolean
  ggufRunMode: GgufRunMode
  modelId: string
  ggufFile: string
  adapter: GgufImageBuildAdapter
}

export async function prepareGgufImageRef({
  selectedRuntime,
  isHuggingFaceGgufModel,
  ggufRunMode,
  modelId,
  ggufFile,
  adapter,
}: PrepareGgufImageRefOptions): Promise<string | undefined> {
  if (selectedRuntime !== 'kaito' || !isHuggingFaceGgufModel || ggufRunMode !== 'build') {
    return undefined
  }

  adapter.notify({
    title: 'Checking Build Infrastructure',
    description: 'Verifying Docker and build tools are available...',
  })

  const infraStatus = await adapter.getInfrastructureStatus()
  if (!infraStatus.ready) {
    throw new Error(buildInfrastructureErrorMessage(infraStatus))
  }

  adapter.notify({
    title: 'Building Image',
    description: `Building GGUF model image for ${modelId}. This may take a few minutes...`,
  })

  const buildResult = await adapter.build({
    modelSource: 'huggingface',
    modelId,
    ggufFile,
  })

  if (!buildResult.success || !buildResult.imageRef) {
    throw new Error(buildResult.error || 'Failed to build model image')
  }

  adapter.notify({
    title: 'Image Built Successfully',
    description: `Image: ${buildResult.imageRef}`,
    variant: 'success',
  })

  return buildResult.imageRef
}

function buildInfrastructureErrorMessage(infraStatus: AikitInfrastructureStatus): string {
  if (infraStatus.error) {
    return infraStatus.error
  }
  if (!infraStatus.builder.running) {
    return 'Docker is not running. Please start Docker and try again.'
  }
  if (!infraStatus.registry.ready) {
    return 'Container registry is not available.'
  }
  return 'Build infrastructure is not ready.'
}
