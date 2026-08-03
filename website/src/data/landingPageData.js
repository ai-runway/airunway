// Content for the landing page. Editing here keeps the page logic clean and
// makes it easy to change copy without touching components.

export const features = [
  {
    emoji: '🚀',
    title: 'One-Click Deploy',
    description:
      'Browse models, check GPU fit, and deploy from the web UI — no YAML required.',
  },
  {
    emoji: '🎯',
    title: 'Unified CRD',
    description:
      'A single ModelDeployment API works across every supported provider and engine.',
  },
  {
    emoji: '🔧',
    title: 'Multiple Engines',
    description:
      'vLLM, SGLang, TensorRT-LLM, and llama.cpp — picked automatically per workload.',
  },
  {
    emoji: '📈',
    title: 'Live Monitoring',
    description:
      'Real-time status, log streaming, and Prometheus metrics built into the dashboard.',
  },
  {
    emoji: '💰',
    title: 'Cost Estimation',
    description:
      'Surface GPU pricing and capacity guidance before you commit to a deployment.',
  },
  {
    emoji: '🌐',
    title: 'Gateway Integration',
    description:
      'Auto-detected Gateway API Inference Extension setup with a unified inference endpoint.',
  },
];

export const providers = [
  {
    name: 'NVIDIA Dynamo',
    href: 'https://github.com/ai-dynamo/dynamo',
    description:
      'GPU-accelerated inference with aggregated or disaggregated serving.',
  },
  {
    name: 'KubeRay',
    href: 'https://github.com/ray-project/kuberay',
    description: 'Ray-based distributed inference.',
  },
  {
    name: 'KAITO',
    href: 'https://github.com/kaito-project/kaito',
    description: 'vLLM (GPU) and llama.cpp (CPU/GPU) support.',
  },
  {
    name: 'LLM-D',
    href: 'https://github.com/llm-d/llm-d',
    description: 'vLLM (GPU) with aggregated or disaggregated serving.',
  },
];

export const ctaLinks = [
  {
    label: 'GitHub',
    href: 'https://github.com/ai-runway/airunway',
  },
  {
    label: 'Issues',
    href: 'https://github.com/ai-runway/airunway/issues',
  },
  {
    label: 'Releases',
    href: 'https://github.com/ai-runway/airunway/releases',
  },
];

export const demoVideoId = 'Pe0sLv7v2FM';
