import logger from './logger';

export interface RetryOptions {
  /** Maximum number of retry attempts (default: 3) */
  maxRetries?: number;
  /** Initial delay in milliseconds (default: 1000) */
  initialDelayMs?: number;
  /** Maximum delay in milliseconds (default: 10000) */
  maxDelayMs?: number;
  /** Exponential backoff factor (default: 2) */
  backoffFactor?: number;
  /** Function to determine if error is retryable (default: retries on network errors) */
  isRetryable?: (error: unknown) => boolean;
  /** Operation name for logging */
  operationName?: string;
}

const DEFAULT_OPTIONS: Required<Omit<RetryOptions, 'isRetryable' | 'operationName'>> = {
  maxRetries: 3,
  initialDelayMs: 1000,
  maxDelayMs: 10000,
  backoffFactor: 2,
};

interface RetryableError {
  statusCode?: number;
  response?: { statusCode?: number };
  code?: string | number;
  message?: string;
}

function getRetryStatusCode(error: RetryableError): number | undefined {
  return error.statusCode
    ?? error.response?.statusCode
    ?? (typeof error.code === 'number' ? error.code : undefined);
}

/**
 * Default function to determine if a Kubernetes API error is retryable
 */
export function isK8sRetryableError(error: unknown): boolean {
  if (!error || typeof error !== 'object') {
    return false;
  }

  const err = error as RetryableError;

  // Get status code from various error formats
  const statusCode = getRetryStatusCode(err);

  // Retry on server errors (5xx) and rate limiting (429)
  if (statusCode && (statusCode >= 500 || statusCode === 429)) {
    return true;
  }

  // Retry on network errors
  const networkErrors = ['ECONNRESET', 'ETIMEDOUT', 'ECONNREFUSED', 'ENOTFOUND', 'EAI_AGAIN'];
  if (typeof err.code === 'string' && networkErrors.includes(err.code)) {
    return true;
  }

  // Retry on specific error messages
  const retryableMessages = [
    'socket hang up',
    'network error',
    'ECONNRESET',
    'ETIMEDOUT',
  ];
  if (err.message && retryableMessages.some((msg) => err.message?.includes(msg))) {
    return true;
  }

  return false;
}

/**
 * Sleep for a given number of milliseconds
 */
function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

/**
 * Execute an async function with retry logic and exponential backoff
 */
export async function withRetry<T>(
  fn: () => Promise<T>,
  options: RetryOptions = {}
): Promise<T> {
  const {
    maxRetries = DEFAULT_OPTIONS.maxRetries,
    initialDelayMs = DEFAULT_OPTIONS.initialDelayMs,
    maxDelayMs = DEFAULT_OPTIONS.maxDelayMs,
    backoffFactor = DEFAULT_OPTIONS.backoffFactor,
    isRetryable = isK8sRetryableError,
    operationName = 'operation',
  } = options;

  let lastError: unknown;
  let delay = initialDelayMs;

  for (let attempt = 0; attempt <= maxRetries; attempt++) {
    try {
      return await fn();
    } catch (error) {
      lastError = error;

      // Don't retry if this is the last attempt or error is not retryable
      if (attempt === maxRetries || !isRetryable(error)) {
        throw error;
      }

      // Log retry attempt
      const statusCode = error && typeof error === 'object'
        ? getRetryStatusCode(error as RetryableError)
        : undefined;
      
      logger.warn(
        {
          operationName,
          attempt: attempt + 1,
          maxRetries,
          delayMs: delay,
          statusCode,
          errorMessage: error instanceof Error ? error.message : String(error),
        },
        `Retrying ${operationName} after transient error (attempt ${attempt + 1}/${maxRetries})`
      );

      // Wait before retrying
      await sleep(delay);

      // Increase delay for next attempt (exponential backoff with jitter)
      const jitter = Math.random() * 0.3 + 0.85; // 0.85 to 1.15
      delay = Math.min(delay * backoffFactor * jitter, maxDelayMs);
    }
  }

  throw lastError;
}

/**
 * Create a retry wrapper with preset options
 */
export function createRetryWrapper(defaultOptions: RetryOptions) {
  return <T>(fn: () => Promise<T>, overrideOptions?: RetryOptions): Promise<T> => {
    return withRetry(fn, { ...defaultOptions, ...overrideOptions });
  };
}

/**
 * Pre-configured retry wrapper for Kubernetes operations
 */
export const k8sRetry = createRetryWrapper({
  maxRetries: 3,
  initialDelayMs: 500,
  maxDelayMs: 5000,
  backoffFactor: 2,
  isRetryable: isK8sRetryableError,
});
