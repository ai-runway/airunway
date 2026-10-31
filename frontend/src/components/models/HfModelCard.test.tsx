import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter } from 'react-router-dom';
import { describe, expect, it, vi } from 'vitest';
import type { HfModelSearchResult } from '@airunway/shared';
import { HfModelCard } from './HfModelCard';

vi.mock('@/hooks/useGpuOperator', () => ({
  useGpuThroughput: () => ({ data: undefined, isLoading: false }),
}));

const model: HfModelSearchResult = {
  id: 'Qwen/Qwen3-Coder-30B-A3B-Instruct',
  author: 'Qwen',
  name: 'Qwen3-Coder-30B-A3B-Instruct',
  downloads: 1234,
  likes: 56,
  pipelineTag: 'text-generation',
  libraryName: 'transformers',
  architectures: ['Qwen3MoeForCausalLM'],
  gated: false,
  parameterCount: 30_000_000_000,
  estimatedGpuMemory: '72 GB',
  estimatedGpuMemoryGb: 72,
  supportedEngines: ['vllm'],
  compatible: true,
};

function renderCard() {
  return render(
    <MemoryRouter>
      <HfModelCard model={model} />
    </MemoryRouter>
  );
}

describe('HfModelCard', () => {
  it('shows the full model identifier when the truncated name is hovered', async () => {
    const user = userEvent.setup();
    renderCard();

    await user.hover(screen.getByRole('heading', { name: model.name }));

    expect(await screen.findByRole('tooltip')).toHaveTextContent(model.id);
  });

  it('makes the full model identifier available from the keyboard', async () => {
    const user = userEvent.setup();
    renderCard();

    const modelName = screen.getByRole('heading', { name: model.name });
    await user.tab();

    expect(modelName).toHaveFocus();
    expect(await screen.findByRole('tooltip')).toHaveTextContent(model.id);
  });
});
