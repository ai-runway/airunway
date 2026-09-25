import { describe, test, expect } from 'bun:test';
import { secretsService } from './secrets';

interface SecretCallArg {
  name: string;
  namespace: string;
}

type MockableSecretsService = {
  coreV1Api: {
    readNamespacedSecret: (arg: SecretCallArg) => Promise<unknown>;
  };
};

const asMockable = (): MockableSecretsService =>
  secretsService as unknown as MockableSecretsService;

describe('SecretsService', () => {
  describe('module exports', () => {
    test('exports secretsService singleton', async () => {
      expect(secretsService).toBeDefined();
      expect(typeof secretsService.distributeHfSecret).toBe('function');
      expect(typeof secretsService.getHfSecretStatus).toBe('function');
      expect(typeof secretsService.deleteHfSecrets).toBe('function');
    });
  });

  describe('HF_SECRET_NAME and namespaces', () => {
    test('service distributes to expected namespaces', async () => {
      // The service should be properly initialized
      // We can't easily test the private TARGET_NAMESPACES constant,
      // but we verify the service methods exist and are callable
      expect(secretsService.distributeHfSecret).toBeInstanceOf(Function);
      expect(secretsService.getHfSecretStatus).toBeInstanceOf(Function);
      expect(secretsService.deleteHfSecrets).toBeInstanceOf(Function);
    });
  });

  describe('getHfSecretStatus', () => {
    test('treats a Kubernetes 404 as an absent secret', async () => {
      const service = asMockable();
      const originalCoreV1Api = service.coreV1Api;
      const namespaces: string[] = [];

      service.coreV1Api = {
        readNamespacedSecret: async ({ namespace }) => {
          namespaces.push(namespace);
          throw { code: 404, message: 'HTTP request failed' };
        },
      };

      try {
        const status = await secretsService.getHfSecretStatus();

        expect(status.configured).toBe(false);
        expect(status.namespaces.every((namespace) => !namespace.exists)).toBe(true);
        expect(namespaces).toEqual([
          'dynamo-system',
          'ray-system',
          'kuberay-system',
          'kaito-workspace',
          'default',
        ]);
      } finally {
        service.coreV1Api = originalCoreV1Api;
      }
    });

    test('propagates unexpected Kubernetes read failures', async () => {
      const service = asMockable();
      const originalCoreV1Api = service.coreV1Api;
      const apiError = { code: 500, message: 'HTTP request failed' };

      service.coreV1Api = {
        readNamespacedSecret: async () => {
          throw apiError;
        },
      };

      try {
        await expect(secretsService.getHfSecretStatus()).rejects.toBe(apiError);
      } finally {
        service.coreV1Api = originalCoreV1Api;
      }
    });
  });
});
