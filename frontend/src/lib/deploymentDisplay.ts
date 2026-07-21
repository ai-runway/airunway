export function getProviderDisplayName(provider?: string): string {
  switch (provider) {
    case 'vllm':
      return 'Direct vLLM'
    case 'dynamo':
      return 'Dynamo'
    case 'kuberay':
      return 'KubeRay'
    case 'kaito':
      return 'KAITO'
    case 'llmd':
      return 'llm-d'
    default:
      return provider || 'Pending'
  }
}

export function getEngineDisplayName(engine?: string): string {
  switch (engine) {
    case 'vllm':
      return 'vLLM'
    case 'sglang':
      return 'SGLang'
    case 'trtllm':
      return 'TensorRT-LLM'
    case 'llamacpp':
      return 'llama.cpp'
    default:
      return engine || 'Pending'
  }
}
