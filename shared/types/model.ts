export type Engine = 'vllm' | 'sglang' | 'trtllm' | 'llamacpp';
export type ModelTask = 'text-generation' | 'image-text-to-text';

export interface Model {
  id: string;                    // HuggingFace model ID (e.g., "Qwen/Qwen3-0.6B")
  name: string;                  // Display name
  description: string;           // Brief description
  size: string;                  // Parameter count (e.g., "0.6B")
  task: ModelTask;               // HuggingFace pipeline tag
  conversational?: boolean;      // Whether model supports chat/instruct format (HF "conversational" tag)
  parameters?: number;           // Actual parameter count
  contextLength?: number;        // Max context length
  license?: string;              // Model license
  supportedEngines: Engine[];    // Compatible inference engines
  minGpuMemory?: string;         // Minimum GPU memory (e.g., "8GB")
  minGpus?: number;              // Minimum GPUs required to run (default: 1)
  gated?: boolean;               // Whether model requires HuggingFace authentication (e.g., Llama, Mistral)
  // Fields populated from HuggingFace search
  estimatedGpuMemory?: string;   // Estimated GPU memory from parameter count (e.g., "16GB")
  estimatedGpuMemoryGb?: number; // Estimated GPU memory in GB (numeric)
  parameterCount?: number;       // Parameter count from HF safetensors metadata
  compatibilityWarnings?: string[]; // Warnings about model compatibility
  fromHfSearch?: boolean;        // Whether this model came from HF search (not curated list)
}

/**
 * Transformer architecture details, read from a model's HuggingFace config.json.
 * Used to size the KV cache when estimating concurrent serving capacity.
 * All fields optional because config.json may be missing or incomplete.
 */
export interface ModelArchitecture {
  numLayers?: number;            // num_hidden_layers
  numKvHeads?: number;           // num_key_value_heads (falls back to num_attention_heads for MHA)
  headDim?: number;              // head_dim (falls back to hidden_size / num_attention_heads)
  maxPositionEmbeddings?: number; // max_position_embeddings (max context length)
  torchDtype?: string;           // e.g. "bfloat16", "float16"
}
