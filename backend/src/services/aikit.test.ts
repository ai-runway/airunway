import { describe, it, expect } from 'bun:test';
import { aikitService, PREMADE_MODELS, type AikitBuildRequest } from './aikit';

describe('AikitService', () => {
  describe('PREMADE_MODELS', () => {
    it('contains expected models', () => {
      expect(PREMADE_MODELS.length).toBeGreaterThanOrEqual(10);

      // Check for key models
      const modelIds = PREMADE_MODELS.map(m => m.id);
      expect(modelIds).toContain('llama3.2:1b');
      expect(modelIds).toContain('llama3.2:3b');
      expect(modelIds).toContain('llama3.1:8b');
      expect(modelIds).toContain('mixtral:8x7b');
      expect(modelIds).toContain('phi4:14b');
    });

    it('all models have required fields', () => {
      for (const model of PREMADE_MODELS) {
        expect(model.id).toBeDefined();
        expect(model.name).toBeDefined();
        expect(model.size).toBeDefined();
        expect(model.image).toBeDefined();
        expect(model.modelName).toBeDefined();
        expect(model.license).toBeDefined();

        // Image should be from ghcr.io/kaito-project/aikit
        expect(model.image).toMatch(/^ghcr\.io\/kaito-project\/aikit\//);
      }
    });
  });

  describe('getPremadeModels', () => {
    it('returns a copy of the models list', () => {
      const models1 = aikitService.getPremadeModels();
      const models2 = aikitService.getPremadeModels();

      expect(models1).not.toBe(models2);
      expect(models1).toEqual(models2);
    });
  });

  describe('getPremadeModel', () => {
    it('returns model by ID', () => {
      const model = aikitService.getPremadeModel('llama3.2:3b');
      expect(model).toBeDefined();
      expect(model?.id).toBe('llama3.2:3b');
      expect(model?.name).toBe('Llama 3.2');
      expect(model?.size).toBe('3B');
    });

    it('returns undefined for unknown model', () => {
      const model = aikitService.getPremadeModel('unknown-model');
      expect(model).toBeUndefined();
    });
  });

  describe('validateBuildRequest', () => {
    it('validates premade model request', () => {
      const request: AikitBuildRequest = {
        modelSource: 'premade',
        premadeModel: 'llama3.2:3b',
      };

      const result = aikitService.validateBuildRequest(request);
      expect(result.valid).toBe(true);
      expect(result.errors).toHaveLength(0);
    });

    it('rejects premade request without premadeModel', () => {
      const request: AikitBuildRequest = {
        modelSource: 'premade',
      };

      const result = aikitService.validateBuildRequest(request);
      expect(result.valid).toBe(false);
      expect(result.errors).toContain('premadeModel is required for premade model source');
    });

    it('rejects unknown premade model', () => {
      const request: AikitBuildRequest = {
        modelSource: 'premade',
        premadeModel: 'unknown-model',
      };

      const result = aikitService.validateBuildRequest(request);
      expect(result.valid).toBe(false);
      expect(result.errors.some(e => e.includes('Unknown premade model'))).toBe(true);
    });

    it('validates HuggingFace request', () => {
      const request: AikitBuildRequest = {
        modelSource: 'huggingface',
        modelId: 'TheBloke/Llama-2-7B-Chat-GGUF',
        ggufFile: 'llama-2-7b-chat.Q4_K_M.gguf',
      };

      const result = aikitService.validateBuildRequest(request);
      expect(result.valid).toBe(true);
      expect(result.errors).toHaveLength(0);
    });

    it('rejects HuggingFace request without modelId', () => {
      const request: AikitBuildRequest = {
        modelSource: 'huggingface',
        ggufFile: 'llama-2-7b-chat.Q4_K_M.gguf',
      };

      const result = aikitService.validateBuildRequest(request);
      expect(result.valid).toBe(false);
      expect(result.errors).toContain('modelId is required for HuggingFace model source');
    });

    it('rejects HuggingFace request without ggufFile', () => {
      const request: AikitBuildRequest = {
        modelSource: 'huggingface',
        modelId: 'TheBloke/Llama-2-7B-Chat-GGUF',
      };

      const result = aikitService.validateBuildRequest(request);
      expect(result.valid).toBe(false);
      expect(result.errors).toContain('ggufFile is required for HuggingFace model source');
    });

    it('rejects non-GGUF file', () => {
      const request: AikitBuildRequest = {
        modelSource: 'huggingface',
        modelId: 'TheBloke/Llama-2-7B-Chat-GGUF',
        ggufFile: 'model.safetensors',
      };

      const result = aikitService.validateBuildRequest(request);
      expect(result.valid).toBe(false);
      expect(result.errors).toContain('ggufFile must be a .gguf file');
    });

    it('rejects invalid model source', () => {
      const request = {
        modelSource: 'invalid' as AikitBuildRequest['modelSource'],
      };

      const result = aikitService.validateBuildRequest(request);
      expect(result.valid).toBe(false);
      expect(result.errors).toContain('modelSource must be either "premade" or "huggingface"');
    });
  });

  describe('buildHuggingFaceUrl', () => {
    it('builds correct URL format', () => {
      const url = aikitService.buildHuggingFaceUrl(
        'TheBloke/Llama-2-7B-Chat-GGUF',
        'llama-2-7b-chat.Q4_K_M.gguf'
      );

      expect(url).toBe(
        'https://huggingface.co/TheBloke/Llama-2-7B-Chat-GGUF/resolve/main/llama-2-7b-chat.Q4_K_M.gguf'
      );
    });

    it('encodes GGUF path segments safely', () => {
      const url = aikitService.buildHuggingFaceUrl(
        'TheBloke/Llama-2-7B-Chat-GGUF',
        'quantized/llama 2 #1.Q4_K_M.gguf'
      );

      expect(url).toBe(
        'https://huggingface.co/TheBloke/Llama-2-7B-Chat-GGUF/resolve/main/quantized/llama%202%20%231.Q4_K_M.gguf'
      );
    });
  });

  describe('extractQuantization', () => {
    it('extracts Q4_K_M quantization', () => {
      expect(aikitService.extractQuantization('llama-2-7b-chat.Q4_K_M.gguf')).toBe('Q4_K_M');
    });

    it('extracts Q5_K_S quantization', () => {
      expect(aikitService.extractQuantization('model.Q5_K_S.gguf')).toBe('Q5_K_S');
    });

    it('extracts Q4_0 quantization', () => {
      expect(aikitService.extractQuantization('model.Q4_0.gguf')).toBe('Q4_0');
    });

    it('extracts Q8 quantization', () => {
      expect(aikitService.extractQuantization('model.Q8.gguf')).toBe('Q8');
    });

    it('extracts IQ quantization', () => {
      expect(aikitService.extractQuantization('model.IQ2_XXS.gguf')).toBe('IQ2_XXS');
    });

    it('extracts F16 quantization', () => {
      expect(aikitService.extractQuantization('model.F16.gguf')).toBe('F16');
    });

    it('returns custom for unknown pattern', () => {
      expect(aikitService.extractQuantization('model.gguf')).toBe('custom');
      expect(aikitService.extractQuantization('weird-file.gguf')).toBe('custom');
    });

    it('is case insensitive', () => {
      expect(aikitService.extractQuantization('model.q4_k_m.gguf')).toBe('Q4_K_M');
    });
  });

  describe('sanitizeImageName', () => {
    it('handles simple model ID', () => {
      expect(aikitService.sanitizeImageName('TheBloke')).toBe('thebloke');
    });

    it('handles model ID with slashes', () => {
      expect(aikitService.sanitizeImageName('TheBloke/Llama-2-7B')).toBe('thebloke-llama-2-7b');
    });

    it('handles special characters', () => {
      expect(aikitService.sanitizeImageName('Org/Model_v1.0')).toBe('org-model-v1-0');
    });

    it('collapses multiple hyphens', () => {
      expect(aikitService.sanitizeImageName('a--b---c')).toBe('a-b-c');
    });

    it('removes leading and trailing hyphens', () => {
      expect(aikitService.sanitizeImageName('-test-')).toBe('test');
    });

    it('limits to 63 characters', () => {
      const longName = 'a'.repeat(100);
      expect(aikitService.sanitizeImageName(longName).length).toBe(63);
    });
  });

  describe('getImageRef', () => {
    it('returns premade image for premade model', () => {
      const request: AikitBuildRequest = {
        modelSource: 'premade',
        premadeModel: 'llama3.2:3b',
      };

      const imageRef = aikitService.getImageRef(request);
      expect(imageRef).toBe('ghcr.io/kaito-project/aikit/llama3.2:3b');
    });

    it('returns null for unknown premade model', () => {
      const request: AikitBuildRequest = {
        modelSource: 'premade',
        premadeModel: 'unknown',
      };

      const imageRef = aikitService.getImageRef(request);
      expect(imageRef).toBeNull();
    });

    it('returns kubelet-accessible registry URL for HuggingFace model', () => {
      const request: AikitBuildRequest = {
        modelSource: 'huggingface',
        modelId: 'TheBloke/Llama-2-7B-Chat-GGUF',
        ggufFile: 'llama-2-7b-chat.Q4_K_M.gguf',
      };

      const imageRef = aikitService.getImageRef(request);
      // Uses localhost:30500 NodePort for kubelet access
      expect(imageRef).toBe(
        'localhost:30500/aikit-thebloke-llama-2-7b-chat-gguf:Q4_K_M'
      );
    });

    it('uses custom image name and tag when provided', () => {
      const request: AikitBuildRequest = {
        modelSource: 'huggingface',
        modelId: 'TheBloke/Llama-2-7B-Chat-GGUF',
        ggufFile: 'llama-2-7b-chat.Q4_K_M.gguf',
        imageName: 'my-custom-image',
        imageTag: 'v1.0',
      };

      const imageRef = aikitService.getImageRef(request);
      // Uses localhost:30500 NodePort for kubelet access
      expect(imageRef).toBe(
        'localhost:30500/my-custom-image:v1.0'
      );
    });

    it('returns null for incomplete HuggingFace request', () => {
      const request: AikitBuildRequest = {
        modelSource: 'huggingface',
        modelId: 'TheBloke/Llama-2-7B-Chat-GGUF',
        // missing ggufFile
      };

      const imageRef = aikitService.getImageRef(request);
      expect(imageRef).toBeNull();
    });
  });

  describe('buildImage', () => {
    it('returns premade image without building', async () => {
      const request: AikitBuildRequest = {
        modelSource: 'premade',
        premadeModel: 'llama3.2:3b',
      };

      const result = await aikitService.buildImage(request);

      expect(result.success).toBe(true);
      expect(result.imageRef).toBe('ghcr.io/kaito-project/aikit/llama3.2:3b');
      expect(result.buildTime).toBe(0);
      expect(result.wasPremade).toBe(true);
      expect(result.error).toBeUndefined();
    });

    it('returns error for invalid request', async () => {
      const request: AikitBuildRequest = {
        modelSource: 'premade',
        // missing premadeModel
      };

      const result = await aikitService.buildImage(request);

      expect(result.success).toBe(false);
      expect(result.error).toContain('Invalid build request');
      expect(result.wasPremade).toBe(false);
    });

    it('returns error for unknown premade model', async () => {
      const request: AikitBuildRequest = {
        modelSource: 'premade',
        premadeModel: 'unknown-model',
      };

      const result = await aikitService.buildImage(request);

      expect(result.success).toBe(false);
      expect(result.error).toContain('Unknown premade model');
    });

    // Note: HuggingFace builds require actual infrastructure
    // These are tested via integration tests, not unit tests
  });
});

describe('AIKit Model Catalog', () => {
  it('all model IDs are unique', () => {
    const ids = PREMADE_MODELS.map(m => m.id);
    const uniqueIds = new Set(ids);
    expect(uniqueIds.size).toBe(ids.length);
  });

  it('all model images are unique', () => {
    const images = PREMADE_MODELS.map(m => m.image);
    const uniqueImages = new Set(images);
    expect(uniqueImages.size).toBe(images.length);
  });

  it('model IDs follow naming convention', () => {
    for (const model of PREMADE_MODELS) {
      // IDs should be lowercase with colons for version
      expect(model.id).toMatch(/^[a-z0-9.-]+:[a-z0-9]+$/);
    }
  });

  it('includes popular model families', () => {
    const names = PREMADE_MODELS.map(m => m.name.toLowerCase());
    
    // Check for key model families
    expect(names.some(n => n.includes('llama'))).toBe(true);
    expect(names.some(n => n.includes('mixtral'))).toBe(true);
    expect(names.some(n => n.includes('phi'))).toBe(true);
    expect(names.some(n => n.includes('gemma'))).toBe(true);
  });
});
