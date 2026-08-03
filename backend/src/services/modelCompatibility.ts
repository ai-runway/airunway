import type { Engine, HfApiModelResult, HfModelSearchResult } from '@airunway/shared';
import { parseParameterCountFromName } from '@airunway/shared';
import { estimateGpuMemory, formatGpuMemory } from './gpuValidation';

/**
 * Architecture allowlists per inference engine
 *
 * These are the model architectures that each engine has been verified to support.
 * Sourced from each engine's model registry as of March 2026.
 * Models with architectures not in this list may still work, but are not guaranteed.
 */
const ENGINE_ARCHITECTURE_ALLOWLIST: Record<Engine, string[]> = {
  vllm: [
    // LLaMA family
    'LlamaForCausalLM',
    'Llama4ForCausalLM',
    'Llama4ForConditionalGeneration',
    'MistralForCausalLM',
    'MistralLarge3ForCausalLM',
    'Mistral3ForConditionalGeneration',
    'Ministral3ForCausalLM',
    'MixtralForCausalLM',
    // Qwen family
    'QWenLMHeadModel',
    'Qwen2ForCausalLM',
    'Qwen2MoeForCausalLM',
    'Qwen3ForCausalLM',
    'Qwen3MoeForCausalLM',
    'Qwen3NextForCausalLM',
    'Qwen2_5_VLForConditionalGeneration',
    'Qwen3VLForConditionalGeneration',
    'Qwen3VLMoeForConditionalGeneration',
    'Qwen3_5ForConditionalGeneration',
    'Qwen3_5MoeForConditionalGeneration',
    // Gemma family
    'GemmaForCausalLM',
    'Gemma2ForCausalLM',
    'Gemma3ForCausalLM',
    'Gemma3ForConditionalGeneration',
    'Gemma3nForCausalLM',
    'Gemma3nForConditionalGeneration',
    // Phi family
    'PhiForCausalLM',
    'Phi3ForCausalLM',
    'PhiMoEForCausalLM',
    'Phi4MMForCausalLM',
    // GPT family
    'GPT2LMHeadModel',
    'GPTNeoForCausalLM',
    'GPTNeoXForCausalLM',
    'GPTJForCausalLM',
    'GPTBigCodeForCausalLM',
    'GptOssForCausalLM',
    // DeepSeek family
    'DeepseekForCausalLM',
    'DeepseekV2ForCausalLM',
    'DeepseekV3ForCausalLM',
    'DeepseekV32ForCausalLM',
    'DeepseekVLV2ForCausalLM',
    // GLM family
    'ChatGLMModel',
    'ChatGLMForConditionalGeneration',
    'Glm4ForCausalLM',
    'Glm4MoeForCausalLM',
    'Glm4MoeLiteForCausalLM',
    'GlmMoeDsaForCausalLM',
    'GLM4VForCausalLM',
    // Kimi / Moonshot
    'KimiK25ForConditionalGeneration',
    'KimiLinearForCausalLM',
    'KimiVLForConditionalGeneration',
    'MoonshotKimiaForCausalLM',
    // Falcon family
    'FalconForCausalLM',
    'FalconMambaForCausalLM',
    'FalconH1ForCausalLM',
    // NVIDIA
    'NemotronForCausalLM',
    'NemotronHForCausalLM',
    // Cohere
    'CohereForCausalLM',
    'Cohere2ForCausalLM',
    'Cohere2VisionForConditionalGeneration',
    // InternLM
    'InternLMForCausalLM',
    'InternLM2ForCausalLM',
    'InternLM3ForCausalLM',
    'InternVLChatModel',
    // MiniCPM
    'MiniCPMForCausalLM',
    'MiniCPM3ForCausalLM',
    // OLMo
    'OlmoForCausalLM',
    'Olmo2ForCausalLM',
    'Olmo3ForCausalLM',
    'OlmoeForCausalLM',
    // Granite
    'GraniteForCausalLM',
    'GraniteMoeForCausalLM',
    'GraniteMoeHybridForCausalLM',
    'GraniteMoeSharedForCausalLM',
    // Other popular architectures
    'StableLmForCausalLM',
    'Starcoder2ForCausalLM',
    'OPTForCausalLM',
    'BloomForCausalLM',
    'MPTForCausalLM',
    'MptForCausalLM',
    'BaichuanForCausalLM',
    'BaiChuanForCausalLM',
    'DbrxForCausalLM',
    'DeciLMForCausalLM',
    'ExaoneForCausalLM',
    'Exaone4ForCausalLM',
    'ExaoneMoEForCausalLM',
    'ArcticForCausalLM',
    'JambaForCausalLM',
    'LagunaForCausalLM',
    'PersimmonForCausalLM',
    'SolarForCausalLM',
    'TeleChat2ForCausalLM',
    'TeleFLMForCausalLM',
    'XverseForCausalLM',
    'Zamba2ForCausalLM',
    'Grok1ForCausalLM',
    'Grok1ModelForCausalLM',
    'OrionForCausalLM',
    'MambaForCausalLM',
    'Mamba2ForCausalLM',
    'BailingMoeForCausalLM',
    'BailingMoeV2ForCausalLM',
    'HunYuanMoEV1ForCausalLM',
    'HunYuanDenseV1ForCausalLM',
    'MiniMaxForCausalLM',
    'MiniMaxM2ForCausalLM',
    'SarvamMoEForCausalLM',
    'SarvamMLAForCausalLM',
    'Lfm2ForCausalLM',
    'Lfm2MoeForCausalLM',
    'AfmoeForCausalLM',
    'ArceeForCausalLM',
    'LongcatFlashForCausalLM',
    'MiMoForCausalLM',
    'MiMoV2FlashForCausalLM',
    'Step3p5ForCausalLM',
    'Dots1ForCausalLM',
    'BambaForCausalLM',
    'Ernie4_5ForCausalLM',
    'Ernie4_5_MoeForCausalLM',
    'Plamo2ForCausalLM',
    'Plamo3ForCausalLM',
    'SeedOssForCausalLM',
  ],
  sglang: [
    // LLaMA family
    'LlamaForCausalLM',
    'Llama4ForCausalLM',
    'MistralForCausalLM',
    'MistralLarge3ForCausalLM',
    'Mistral3ForConditionalGeneration',
    'Ministral3ForCausalLM',
    'MixtralForCausalLM',
    // Qwen family
    'QWenLMHeadModel',
    'Qwen2ForCausalLM',
    'Qwen2MoeForCausalLM',
    'Qwen3ForCausalLM',
    'Qwen3MoeForCausalLM',
    'Qwen3VLForConditionalGeneration',
    'Qwen3_5ForConditionalGeneration',
    'Qwen3_5MoeForConditionalGeneration',
    // Gemma family
    'GemmaForCausalLM',
    'Gemma2ForCausalLM',
    'Gemma3ForCausalLM',
    'Gemma3ForConditionalGeneration',
    'Gemma3nForCausalLM',
    // Phi family
    'PhiForCausalLM',
    'Phi3ForCausalLM',
    'PhiMoEForCausalLM',
    'Phi4MMForCausalLM',
    // GPT family
    'GPT2LMHeadModel',
    'GPTNeoXForCausalLM',
    'GPTBigCodeForCausalLM',
    'GPTJForCausalLM',
    'GptOssForCausalLM',
    // DeepSeek family
    'DeepseekV2ForCausalLM',
    'DeepseekV3ForCausalLM',
    'DeepseekV32ForCausalLM',
    // GLM family
    'ChatGLMModel',
    'Glm4ForCausalLM',
    'Glm4MoeForCausalLM',
    'Glm4MoeLiteForCausalLM',
    'GlmMoeDsaForCausalLM',
    // Kimi / Moonshot
    'KimiK25ForConditionalGeneration',
    'KimiLinearForCausalLM',
    // Falcon
    'FalconH1ForCausalLM',
    // NVIDIA
    'NemotronHForCausalLM',
    // Cohere
    'CohereForCausalLM',
    'Cohere2ForCausalLM',
    // InternLM
    'InternLM2ForCausalLM',
    'InternLM3ForCausalLM',
    // MiniCPM
    'MiniCPMForCausalLM',
    'MiniCPM3ForCausalLM',
    // OLMo
    'OlmoForCausalLM',
    'Olmo2ForCausalLM',
    'OlmoeForCausalLM',
    // Granite
    'GraniteForCausalLM',
    'GraniteMoeForCausalLM',
    'GraniteMoeHybridForCausalLM',
    // Other
    'StableLmForCausalLM',
    'Starcoder2ForCausalLM',
    'OPTForCausalLM',
    'BaichuanForCausalLM',
    'DbrxForCausalLM',
    'ExaoneForCausalLM',
    'Exaone4ForCausalLM',
    'ExaoneMoEForCausalLM',
    'SolarForCausalLM',
    'PersimmonForCausalLM',
    'XverseForCausalLM',
    'OrionForCausalLM',
    'TeleFLMForCausalLM',
    'Grok1ForCausalLM',
    'Grok1ModelForCausalLM',
    'BailingMoeForCausalLM',
    'BailingMoeV2ForCausalLM',
    'HunYuanMoEV1ForCausalLM',
    'HunYuanDenseV1ForCausalLM',
    'SarvamMoEForCausalLM',
    'SarvamMLAForCausalLM',
    'Lfm2ForCausalLM',
    'AfmoeForCausalLM',
    'ArceeForCausalLM',
    'LongcatFlashForCausalLM',
    'MiMoForCausalLM',
    'MiMoV2FlashForCausalLM',
    'Step3p5ForCausalLM',
  ],
  trtllm: [
    // TensorRT-LLM — optimized subset via MODEL_MAP
    'LlamaForCausalLM',
    'MistralForCausalLM',
    'MixtralForCausalLM',
    'Qwen2ForCausalLM',
    'Qwen2MoeForCausalLM',
    'Qwen3ForCausalLM',
    'Qwen3MoeForCausalLM',
    'QWenLMHeadModel',
    'QWenForCausalLM',
    'GPT2LMHeadModel',
    'GPTBigCodeForCausalLM',
    'GPTNeoXForCausalLM',
    'GPTJForCausalLM',
    'NemotronForCausalLM',
    'FalconForCausalLM',
    'PhiForCausalLM',
    'Phi3ForCausalLM',
    'PhiMoEForCausalLM',
    'Phi4MMForCausalLM',
    'GemmaForCausalLM',
    'RecurrentGemmaForCausalLM',
    'BloomForCausalLM',
    'MPTForCausalLM',
    'MptForCausalLM',
    'OPTForCausalLM',
    'BaichuanForCausalLM',
    'BaiChuanForCausalLM',
    'ChatGLMModel',
    'ChatGLMForCausalLM',
    'ChatGLMForConditionalGeneration',
    'CohereForCausalLM',
    'DbrxForCausalLM',
    'DeciLMForCausalLM',
    'DeepseekForCausalLM',
    'DeepseekV2ForCausalLM',
    'ExaoneForCausalLM',
    'InternLMForCausalLM',
    'InternLM2ForCausalLM',
    'GraniteForCausalLM',
    'GraniteMoeForCausalLM',
    'ArcticForCausalLM',
    'Grok1ModelForCausalLM',
    'MambaForCausalLM',
    'Starcoder2ForCausalLM',
    'JAISLMHeadModel',
  ],
  llamacpp: [
    // llama.cpp GGUF support — sourced from LLM_ARCH enum
    'LlamaForCausalLM',
    'Llama4ForCausalLM',
    'MistralForCausalLM',
    'Mistral3ForConditionalGeneration',
    'MixtralForCausalLM',
    'Qwen2ForCausalLM',
    'Qwen2MoeForCausalLM',
    'Qwen3ForCausalLM',
    'Qwen3MoeForCausalLM',
    'QWenLMHeadModel',
    'GPT2LMHeadModel',
    'GPTNeoForCausalLM',
    'GPTNeoXForCausalLM',
    'GptOssForCausalLM',
    'PhiForCausalLM',
    'Phi3ForCausalLM',
    'PhiMoEForCausalLM',
    'GemmaForCausalLM',
    'Gemma2ForCausalLM',
    'Gemma3ForCausalLM',
    'Gemma3nForCausalLM',
    'FalconForCausalLM',
    'FalconH1ForCausalLM',
    'StableLmForCausalLM',
    'Starcoder2ForCausalLM',
    'BloomForCausalLM',
    'MPTForCausalLM',
    'MptForCausalLM',
    'InternLMForCausalLM',
    'InternLM2ForCausalLM',
    'DeepseekForCausalLM',
    'DeepseekV2ForCausalLM',
    'ChatGLMModel',
    'Glm4ForCausalLM',
    'Glm4MoeForCausalLM',
    'OlmoForCausalLM',
    'Olmo2ForCausalLM',
    'OlmoeForCausalLM',
    'MiniCPMForCausalLM',
    'MiniCPM3ForCausalLM',
    'BaichuanForCausalLM',
    'CohereForCausalLM',
    'Cohere2ForCausalLM',
    'DbrxForCausalLM',
    'GraniteForCausalLM',
    'GraniteMoeForCausalLM',
    'ExaoneForCausalLM',
    'Exaone4ForCausalLM',
    'JambaForCausalLM',
    'MambaForCausalLM',
    'Mamba2ForCausalLM',
    'ArcticForCausalLM',
    'ArceeForCausalLM',
    'NemotronForCausalLM',
    'NemotronHForCausalLM',
    'KimiLinearForCausalLM',
    'BailingMoeForCausalLM',
    'Ernie4_5ForCausalLM',
    'Ernie4_5_MoeForCausalLM',
    'HunYuanMoEV1ForCausalLM',
    'HunYuanDenseV1ForCausalLM',
    'AfmoeForCausalLM',
    'MiniMaxM2ForCausalLM',
    'Lfm2ForCausalLM',
    'Lfm2MoeForCausalLM',
    'Dots1ForCausalLM',
    'Step3p5ForCausalLM',
    'SeedOssForCausalLM',
    'Plamo2ForCausalLM',
    'Plamo3ForCausalLM',
    'MiMoV2FlashForCausalLM',
  ],
};

/**
 * Supported pipeline tags for text generation models
 * Includes image-text-to-text since many modern multimodal models (Llama 4, Gemma 3,
 * Kimi K2.5, Ministral 3, etc.) are tagged this way on HuggingFace but work for
 * text generation with vLLM/SGLang.
 */
const SUPPORTED_PIPELINE_TAGS = [
  'text-generation',
  'text2text-generation',
  'conversational',
  'image-text-to-text',
];

/**
 * Patterns to infer architecture from model ID/name
 * Used for gated models where config metadata is not available
 * Order matters - more specific patterns should come first
 */
const ARCHITECTURE_INFERENCE_PATTERNS: Array<{ pattern: RegExp; architecture: string }> = [
  // LLaMA family (case insensitive, can be part of word like "TinyLlama")
  { pattern: /llama[-_]?4/i, architecture: 'Llama4ForCausalLM' },
  { pattern: /llama[-_]?3/i, architecture: 'LlamaForCausalLM' },
  { pattern: /llama[-_]?2/i, architecture: 'LlamaForCausalLM' },
  { pattern: /llama/i, architecture: 'LlamaForCausalLM' },
  // Mistral family (mixtral must come before mistral, ministral before mistral)
  { pattern: /\bmixtral/i, architecture: 'MixtralForCausalLM' },
  { pattern: /\bministral/i, architecture: 'Ministral3ForCausalLM' },
  { pattern: /\bmistral[-_]?large/i, architecture: 'MistralLarge3ForCausalLM' },
  { pattern: /\bmistral/i, architecture: 'MistralForCausalLM' },
  // Qwen family (more specific versions first)
  { pattern: /\bqwen[-_.]?3[-_.]?5/i, architecture: 'Qwen3_5ForConditionalGeneration' },
  { pattern: /\bqwen[-_]?3.*moe/i, architecture: 'Qwen3MoeForCausalLM' },
  { pattern: /\bqwen[-_]?3/i, architecture: 'Qwen3ForCausalLM' },
  { pattern: /\bqwen[-_]?2.*moe/i, architecture: 'Qwen2MoeForCausalLM' },
  { pattern: /\bqwen/i, architecture: 'Qwen2ForCausalLM' },
  // Gemma family
  { pattern: /\bgemma[-_]?3n/i, architecture: 'Gemma3nForCausalLM' },
  { pattern: /\bgemma[-_]?3/i, architecture: 'Gemma3ForCausalLM' },
  { pattern: /\bgemma[-_]?2(?:-|$)/i, architecture: 'Gemma2ForCausalLM' },
  { pattern: /\bgemma2\b/i, architecture: 'Gemma2ForCausalLM' },
  { pattern: /\bgemma\b/i, architecture: 'GemmaForCausalLM' },
  // Phi family
  { pattern: /\bphi[-_]?4/i, architecture: 'Phi4MMForCausalLM' },
  { pattern: /\bphi[-_]?3/i, architecture: 'Phi3ForCausalLM' },
  { pattern: /\bphi/i, architecture: 'PhiForCausalLM' },
  // Kimi / Moonshot
  { pattern: /\bkimi[-_]?k2/i, architecture: 'KimiK25ForConditionalGeneration' },
  { pattern: /\bkimi/i, architecture: 'KimiLinearForCausalLM' },
  // Falcon
  { pattern: /\bfalcon[-_]?h/i, architecture: 'FalconH1ForCausalLM' },
  { pattern: /\bfalcon/i, architecture: 'FalconForCausalLM' },
  // DeepSeek
  { pattern: /\bdeepseek[-_]?v3/i, architecture: 'DeepseekV3ForCausalLM' },
  { pattern: /\bdeepseek/i, architecture: 'DeepseekV2ForCausalLM' },
  // GLM
  { pattern: /\bglm[-_]?4/i, architecture: 'Glm4ForCausalLM' },
  { pattern: /\bchatglm/i, architecture: 'ChatGLMModel' },
  // Nemotron
  { pattern: /\bnemotron/i, architecture: 'NemotronForCausalLM' },
  // Granite
  { pattern: /\bgranite/i, architecture: 'GraniteForCausalLM' },
  // OLMo
  { pattern: /\bolmo[-_]?2/i, architecture: 'Olmo2ForCausalLM' },
  { pattern: /\bolmo/i, architecture: 'OlmoForCausalLM' },
  // InternLM
  { pattern: /\binternlm[-_]?3/i, architecture: 'InternLM3ForCausalLM' },
  { pattern: /\binternlm/i, architecture: 'InternLM2ForCausalLM' },
  // Cohere
  { pattern: /\bcohere[-_]?2/i, architecture: 'Cohere2ForCausalLM' },
  { pattern: /\bcohere\b|command[-_]?r/i, architecture: 'CohereForCausalLM' },
  // Grok
  { pattern: /\bgrok/i, architecture: 'Grok1ForCausalLM' },
  // Jamba
  { pattern: /\bjamba/i, architecture: 'JambaForCausalLM' },
  // MiniCPM
  { pattern: /\bminicpm/i, architecture: 'MiniCPMForCausalLM' },
  // Exaone
  { pattern: /\bexaone/i, architecture: 'ExaoneForCausalLM' },
  // Starcoder
  { pattern: /\bstarcoder/i, architecture: 'Starcoder2ForCausalLM' },
  // GPT-OSS
  { pattern: /\bgpt[-_]?oss/i, architecture: 'GptOssForCausalLM' },
];

/**
 * Infer architecture from model ID when config is not available
 * This is used for gated models where the HuggingFace API doesn't return full metadata
 */
export function inferArchitectureFromModelId(modelId: string): string[] {
  for (const { pattern, architecture } of ARCHITECTURE_INFERENCE_PATTERNS) {
    if (pattern.test(modelId)) {
      return [architecture];
    }
  }
  return [];
}

/**
 * Check which engines support a given architecture
 */
export function getSupportedEngines(architectures: string[]): Engine[] {
  const engines: Engine[] = [];
  
  for (const engine of ['vllm', 'sglang', 'trtllm', 'llamacpp'] as Engine[]) {
    const allowlist = ENGINE_ARCHITECTURE_ALLOWLIST[engine];
    const isSupported = architectures.some(arch => allowlist.includes(arch));
    if (isSupported) {
      engines.push(engine);
    }
  }
  
  return engines;
}

/**
 * Check if a model's pipeline tag is compatible with our engines
 */
export function isPipelineTagCompatible(pipelineTag?: string): boolean {
  if (!pipelineTag) return false;
  return SUPPORTED_PIPELINE_TAGS.includes(pipelineTag);
}

/**
 * Get incompatibility reason for a model
 */
export function getIncompatibilityReason(
  pipelineTag?: string,
  libraryName?: string,
  architectures?: string[],
  supportedEngines?: Engine[]
): string | undefined {
  if (!pipelineTag) {
    return 'Model has no pipeline tag';
  }
  
  if (!isPipelineTagCompatible(pipelineTag)) {
    return `Pipeline tag "${pipelineTag}" is not supported for inference`;
  }
  
  if (libraryName && libraryName !== 'transformers' && libraryName !== 'vllm') {
    return `Library "${libraryName}" is not supported`;
  }
  
  if (!architectures || architectures.length === 0) {
    return 'Model architecture is unknown';
  }
  
  if (!supportedEngines || supportedEngines.length === 0) {
    return `Architecture "${architectures[0]}" is not supported by any engine`;
  }
  
  return undefined;
}

/**
 * Extract parameter count from HuggingFace model metadata
 */
export function extractParameterCount(model: HfApiModelResult): number | undefined {
  // Try safetensors metadata first (most accurate)
  if (model.safetensors?.total) {
    return model.safetensors.total;
  }
  
  // Try parameters map from safetensors
  if (model.safetensors?.parameters) {
    const params = model.safetensors.parameters;
    // Sum all parameter counts (handles sharded models)
    const total = Object.values(params).reduce<number>((sum, count) => sum + (count as number), 0);
    if (total > 0) return total;
  }
  
  // Fallback: parse parameter count from model name
  // This handles gated models and cases where safetensors metadata is not available
  return parseParameterCountFromName(model.id);
}

/**
 * Process a raw HuggingFace API result into our search result format
 */
export function processHfModel(model: HfApiModelResult): HfModelSearchResult {
  // Get architectures from config, or infer from model ID for gated models
  let architectures = model.config?.architectures || [];
  const isGated = model.gated === true || model.gated === 'auto' || model.gated === 'manual';
  
  // For gated models without architecture info, try to infer it from the model ID
  // The HuggingFace API doesn't return full metadata for gated models without auth
  if (architectures.length === 0) {
    architectures = inferArchitectureFromModelId(model.id);
  }
  
  // Check if this is a GGUF model - GGUF models only support llama.cpp
  // Detection: HuggingFace API sets library_name to 'gguf', or model ID contains 'gguf'
  const libraryName = model.library_name || '';
  const isGgufModel = libraryName === 'gguf' || 
                      model.id.toLowerCase().includes('gguf') ||
                      model.id.toLowerCase().includes('-gguf');
  
  // GGUF models only support llamacpp
  // Non-GGUF models exclude llamacpp (llama.cpp requires GGUF format, not safetensors)
  const supportedEngines: Engine[] = isGgufModel 
    ? ['llamacpp'] 
    : getSupportedEngines(architectures).filter(e => e !== 'llamacpp');
  const pipelineTag = model.pipeline_tag || '';
  
  // For gated models without metadata, assume they're compatible if we could infer architecture
  // This is because gated models (like meta-llama) are typically text-generation models
  const hasInferredCompatibility = architectures.length > 0 && supportedEngines.length > 0;
  // GGUF models are always compatible if detected as such
  const hasGgufCompatibility = isGgufModel && supportedEngines.length > 0;
  const hasExplicitCompatibility = 
    isPipelineTagCompatible(pipelineTag) &&
    supportedEngines.length > 0 &&
    (libraryName === 'transformers' || libraryName === 'vllm' || libraryName === 'gguf' || libraryName === '');
  
  // A model is compatible if either:
  // 1. It has explicit metadata confirming compatibility
  // 2. It's missing metadata but we could infer a supported architecture (likely a gated model)
  // 3. It's a GGUF model (always compatible with llama.cpp)
  const compatible = hasExplicitCompatibility || hasInferredCompatibility || hasGgufCompatibility;
  
  const incompatibilityReason = compatible 
    ? undefined 
    : getIncompatibilityReason(pipelineTag, libraryName, architectures, supportedEngines);
  
  const parameterCount = extractParameterCount(model);
  const gpuMemory = parameterCount ? estimateGpuMemory(parameterCount) : undefined;
  
  // Parse author from model ID (format: "author/model-name")
  const [author, ...nameParts] = model.id.split('/');
  const name = nameParts.join('/') || model.id;
  
  return {
    id: model.id,
    author: author || 'unknown',
    name: name,
    downloads: model.downloads || 0,
    likes: model.likes || 0,
    pipelineTag,
    libraryName,
    architectures,
    gated: isGated,
    parameterCount,
    estimatedGpuMemory: gpuMemory ? formatGpuMemory(gpuMemory) : undefined,
    estimatedGpuMemoryGb: gpuMemory,
    supportedEngines,
    compatible,
    incompatibilityReason,
  };
}

/**
 * Filter and process HuggingFace API results
 * Only returns compatible models
 */
export function filterCompatibleModels(models: HfApiModelResult[]): HfModelSearchResult[] {
  return models
    .map(processHfModel)
    .filter(model => model.compatible);
}

/**
 * Get architecture allowlist for a specific engine
 */
export function getEngineArchitectures(engine: Engine): string[] {
  return [...ENGINE_ARCHITECTURE_ALLOWLIST[engine]];
}
