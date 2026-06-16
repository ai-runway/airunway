import { describe, expect, it, vi } from 'vitest'

import { prepareGgufImageRef, type GgufImageBuildAdapter } from './deploymentFormSubmit'

function readyInfrastructure() {
  return {
    ready: true,
    registry: { ready: true, url: 'registry.local' },
    builder: { exists: true, running: true, name: 'buildkit' },
  }
}

function adapter(overrides: Partial<GgufImageBuildAdapter> = {}): GgufImageBuildAdapter {
  return {
    notify: vi.fn(),
    getInfrastructureStatus: vi.fn(async () => readyInfrastructure()),
    build: vi.fn(async () => ({
      success: true,
      imageRef: 'registry.local/model:tag',
      buildTime: 12,
      wasPremade: false,
      message: 'built',
    })),
    ...overrides,
  }
}

describe('prepareGgufImageRef', () => {
  it('does nothing when the selected path does not require a GGUF image build', async () => {
    const buildAdapter = adapter()

    await expect(prepareGgufImageRef({
      selectedRuntime: 'dynamo',
      isHuggingFaceGgufModel: true,
      ggufRunMode: 'build',
      modelId: 'org/model',
      ggufFile: 'model.gguf',
      adapter: buildAdapter,
    })).resolves.toBeUndefined()

    expect(buildAdapter.getInfrastructureStatus).not.toHaveBeenCalled()
    expect(buildAdapter.build).not.toHaveBeenCalled()
    expect(buildAdapter.notify).not.toHaveBeenCalled()
  })

  it('checks infrastructure, builds the image, and returns its image reference', async () => {
    const buildAdapter = adapter()

    await expect(prepareGgufImageRef({
      selectedRuntime: 'kaito',
      isHuggingFaceGgufModel: true,
      ggufRunMode: 'build',
      modelId: 'org/model',
      ggufFile: 'model.Q4_K_M.gguf',
      adapter: buildAdapter,
    })).resolves.toBe('registry.local/model:tag')

    expect(buildAdapter.notify).toHaveBeenNthCalledWith(1, {
      title: 'Checking Build Infrastructure',
      description: 'Verifying Docker and build tools are available...',
    })
    expect(buildAdapter.build).toHaveBeenCalledWith({
      modelSource: 'huggingface',
      modelId: 'org/model',
      ggufFile: 'model.Q4_K_M.gguf',
    })
    expect(buildAdapter.notify).toHaveBeenLastCalledWith({
      title: 'Image Built Successfully',
      description: 'Image: registry.local/model:tag',
      variant: 'success',
    })
  })

  it('uses the infrastructure error when preflight reports one', async () => {
    const buildAdapter = adapter({
      getInfrastructureStatus: vi.fn(async () => ({
        ready: false,
        registry: { ready: false },
        builder: { exists: false, running: false },
        error: 'BuildKit is unavailable',
      })),
    })

    await expect(prepareGgufImageRef({
      selectedRuntime: 'kaito',
      isHuggingFaceGgufModel: true,
      ggufRunMode: 'build',
      modelId: 'org/model',
      ggufFile: 'model.gguf',
      adapter: buildAdapter,
    })).rejects.toThrow('BuildKit is unavailable')
    expect(buildAdapter.build).not.toHaveBeenCalled()
  })

  it('explains common infrastructure failure modes when no explicit error is present', async () => {
    await expect(prepareGgufImageRef({
      selectedRuntime: 'kaito',
      isHuggingFaceGgufModel: true,
      ggufRunMode: 'build',
      modelId: 'org/model',
      ggufFile: 'model.gguf',
      adapter: adapter({
        getInfrastructureStatus: vi.fn(async () => ({
          ready: false,
          registry: { ready: true },
          builder: { exists: true, running: false },
        })),
      }),
    })).rejects.toThrow('Docker is not running. Please start Docker and try again.')

    await expect(prepareGgufImageRef({
      selectedRuntime: 'kaito',
      isHuggingFaceGgufModel: true,
      ggufRunMode: 'build',
      modelId: 'org/model',
      ggufFile: 'model.gguf',
      adapter: adapter({
        getInfrastructureStatus: vi.fn(async () => ({
          ready: false,
          registry: { ready: false },
          builder: { exists: true, running: true },
        })),
      }),
    })).rejects.toThrow('Container registry is not available.')
  })

  it('throws the build error when the image build fails', async () => {
    const buildAdapter = adapter({
      build: vi.fn(async () => ({
        success: false,
        imageRef: '',
        buildTime: 1,
        wasPremade: false,
        message: 'failed',
        error: 'Image build failed',
      })),
    })

    await expect(prepareGgufImageRef({
      selectedRuntime: 'kaito',
      isHuggingFaceGgufModel: true,
      ggufRunMode: 'build',
      modelId: 'org/model',
      ggufFile: 'model.gguf',
      adapter: buildAdapter,
    })).rejects.toThrow('Image build failed')
  })
})
