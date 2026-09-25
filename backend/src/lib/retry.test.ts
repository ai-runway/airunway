import { describe, test, expect, mock } from 'bun:test';
import { isK8sRetryableError, withRetry, createRetryWrapper } from './retry';

describe('isK8sRetryableError', () => {
  test('returns false for null/undefined', () => {
    expect(isK8sRetryableError(null)).toBe(false);
    expect(isK8sRetryableError(undefined)).toBe(false);
  });

  test('returns false for non-object', () => {
    expect(isK8sRetryableError('error')).toBe(false);
    expect(isK8sRetryableError(42)).toBe(false);
  });

  test('returns true for 5xx status codes', () => {
    expect(isK8sRetryableError({ statusCode: 500 })).toBe(true);
    expect(isK8sRetryableError({ statusCode: 502 })).toBe(true);
    expect(isK8sRetryableError({ statusCode: 503 })).toBe(true);
    expect(isK8sRetryableError({ statusCode: 504 })).toBe(true);
  });

  test('returns true for 429 rate limiting', () => {
    expect(isK8sRetryableError({ statusCode: 429 })).toBe(true);
    expect(isK8sRetryableError({ response: { statusCode: 429 } })).toBe(true);
  });

  test('handles numeric Kubernetes client code values', () => {
    expect(isK8sRetryableError({ code: 503 })).toBe(true);
    expect(isK8sRetryableError({ code: 429 })).toBe(true);
    expect(isK8sRetryableError({ code: 404 })).toBe(false);
  });

  test('returns false for 4xx client errors', () => {
    expect(isK8sRetryableError({ statusCode: 400 })).toBe(false);
    expect(isK8sRetryableError({ statusCode: 404 })).toBe(false);
    expect(isK8sRetryableError({ statusCode: 403 })).toBe(false);
  });

  test('returns true for network error codes', () => {
    expect(isK8sRetryableError({ code: 'ECONNRESET' })).toBe(true);
    expect(isK8sRetryableError({ code: 'ETIMEDOUT' })).toBe(true);
    expect(isK8sRetryableError({ code: 'ECONNREFUSED' })).toBe(true);
    expect(isK8sRetryableError({ code: 'ENOTFOUND' })).toBe(true);
    expect(isK8sRetryableError({ code: 'EAI_AGAIN' })).toBe(true);
  });

  test('returns true for network error messages', () => {
    expect(isK8sRetryableError({ message: 'socket hang up' })).toBe(true);
    expect(isK8sRetryableError({ message: 'network error occurred' })).toBe(true);
    expect(isK8sRetryableError({ message: 'connection ECONNRESET' })).toBe(true);
    expect(isK8sRetryableError({ message: 'request ETIMEDOUT' })).toBe(true);
  });

  test('returns false for regular errors', () => {
    expect(isK8sRetryableError({ message: 'invalid resource' })).toBe(false);
    expect(isK8sRetryableError(new Error('some error'))).toBe(false);
  });

  test('handles nested response statusCode', () => {
    expect(isK8sRetryableError({ response: { statusCode: 500 } })).toBe(true);
    expect(isK8sRetryableError({ response: { statusCode: 404 } })).toBe(false);
  });
});

describe('withRetry', () => {
  test('returns result on success', async () => {
    const fn = mock(() => Promise.resolve('success'));
    const result = await withRetry(fn, { maxRetries: 3 });
    expect(result).toBe('success');
    expect(fn).toHaveBeenCalledTimes(1);
  });

  test('retries on retryable error', async () => {
    let attempts = 0;
    const fn = mock(async () => {
      attempts++;
      if (attempts < 3) {
        throw { statusCode: 503 };
      }
      return 'success';
    });

    const result = await withRetry(fn, {
      maxRetries: 3,
      initialDelayMs: 1,
      maxDelayMs: 10,
    });

    expect(result).toBe('success');
    expect(fn).toHaveBeenCalledTimes(3);
  });

  test('throws immediately on non-retryable error', async () => {
    const fn = mock(async () => {
      throw { statusCode: 404, message: 'Not found' };
    });

    await expect(
      withRetry(fn, { maxRetries: 3, initialDelayMs: 1 })
    ).rejects.toEqual({ statusCode: 404, message: 'Not found' });

    expect(fn).toHaveBeenCalledTimes(1);
  });

  test('throws after max retries exceeded', async () => {
    const fn = mock(async () => {
      throw { statusCode: 503 };
    });

    await expect(
      withRetry(fn, { maxRetries: 2, initialDelayMs: 1, maxDelayMs: 5 })
    ).rejects.toEqual({ statusCode: 503 });

    expect(fn).toHaveBeenCalledTimes(3); // initial + 2 retries
  });

  test('uses custom isRetryable function', async () => {
    let attempts = 0;
    const fn = mock(async () => {
      attempts++;
      if (attempts < 2) {
        throw new Error('custom error');
      }
      return 'success';
    });

    const result = await withRetry(fn, {
      maxRetries: 3,
      initialDelayMs: 1,
      isRetryable: (err) => err instanceof Error && err.message === 'custom error',
    });

    expect(result).toBe('success');
    expect(fn).toHaveBeenCalledTimes(2);
  });

  test('uses default options', async () => {
    const fn = mock(() => Promise.resolve('success'));
    const result = await withRetry(fn);
    expect(result).toBe('success');
  });
});

describe('createRetryWrapper', () => {
  test('creates wrapper with preset options', async () => {
    const wrapper = createRetryWrapper({
      maxRetries: 2,
      initialDelayMs: 1,
      isRetryable: () => true,
    });

    let attempts = 0;
    const fn = async () => {
      attempts++;
      if (attempts < 2) {
        throw new Error('retry me');
      }
      return 'wrapped success';
    };

    const result = await wrapper(fn);
    expect(result).toBe('wrapped success');
    expect(attempts).toBe(2);
  });

  test('allows overriding preset options', async () => {
    const wrapper = createRetryWrapper({ maxRetries: 5 });

    const fn = mock(async () => {
      throw { statusCode: 503 };
    });

    await expect(
      wrapper(fn, { maxRetries: 1, initialDelayMs: 1 })
    ).rejects.toBeDefined();

    expect(fn).toHaveBeenCalledTimes(2); // override to 1 retry = 2 attempts
  });
});
