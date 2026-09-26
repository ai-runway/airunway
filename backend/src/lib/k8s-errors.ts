import logger from './logger';

/**
 * Kubernetes API error response structure
 */
export interface K8sApiError {
  statusCode?: number;
  response?: {
    statusCode?: number;
    body?: K8sErrorBody | string;
  };
  body?: K8sErrorBody | string;
  message?: string;
  code?: string | number;
}

/**
 * Kubernetes error body structure (Status object)
 */
export interface K8sErrorBody {
  kind?: string;
  apiVersion?: string;
  status?: string;
  message?: string;
  reason?: string;
  details?: {
    name?: string;
    group?: string;
    kind?: string;
    causes?: Array<{
      reason?: string;
      message?: string;
      field?: string;
    }>;
  };
  code?: number;
}

/**
 * User-friendly error messages for common Kubernetes errors
 */
const ERROR_MESSAGES: Record<string, string> = {
  'Forbidden': 'Permission denied. Check if the user has the required RBAC permissions.',
  'NotFound': 'Resource not found. The CRD or namespace may not exist.',
  'AlreadyExists': 'A deployment with this name already exists.',
  'Invalid': 'Invalid configuration. Check the deployment parameters.',
  'Conflict': 'Resource conflict. The resource was modified by another process.',
  'Unauthorized': 'Unauthorized. Check your cluster credentials.',
  'ServiceUnavailable': 'Kubernetes API server is unavailable. Try again later.',
  'InternalError': 'Kubernetes API server internal error. Try again later.',
};

/**
 * Extract a detailed, user-friendly error message from a Kubernetes API error
 */
export function extractK8sErrorMessage(error: unknown): string {
  if (!error) {
    return 'Unknown error occurred';
  }

  const k8sError = error as K8sApiError;
  
  const body = parseK8sErrorBody(k8sError);
  if (typeof body === 'string') return body;
  const parsedBody = body;

  // If we have a K8s Status body, extract detailed information
  if (parsedBody) {
    const parts: string[] = [];

    // Get the main message
    if (parsedBody.message) {
      parts.push(parsedBody.message);
    }

    // Add field-specific causes
    if (parsedBody.details?.causes && parsedBody.details.causes.length > 0) {
      const causeMessages = parsedBody.details.causes
        .map((cause: { reason?: string; message?: string; field?: string }) => {
          if (cause.field && cause.message) {
            return `${cause.field}: ${cause.message}`;
          }
          return cause.message || cause.reason;
        })
        .filter(Boolean);
      
      if (causeMessages.length > 0) {
        parts.push(`Details: ${causeMessages.join('; ')}`);
      }
    }

    if (parts.length > 0) {
      return parts.join(' ');
    }

    // Fall back to reason-based message
    if (parsedBody.reason && ERROR_MESSAGES[parsedBody.reason]) {
      return ERROR_MESSAGES[parsedBody.reason];
    }
  }

  // Get status code for more context
  const statusCode = getK8sStatusCode(k8sError);

  // Fall back to the raw message with status code context
  if (k8sError.message) {
    if (k8sError.message === 'HTTP request failed' && statusCode) {
      return getStatusCodeMessage(statusCode);
    }
    return k8sError.message;
  }

  if (statusCode) {
    return getStatusCodeMessage(statusCode);
  }

  return 'Unknown Kubernetes API error';
}

/**
 * Get a human-readable message for an HTTP status code
 */
function getStatusCodeMessage(statusCode: number): string {
  switch (statusCode) {
    case 400:
      return 'Invalid request. Check the deployment configuration.';
    case 401:
      return 'Authentication failed. Check your cluster credentials.';
    case 403:
      return 'Permission denied. Check if you have the required RBAC permissions to create deployments.';
    case 404:
      return 'Resource not found. The CRD or namespace may not exist. Check if the runtime is installed.';
    case 409:
      return 'A deployment with this name already exists.';
    case 422:
      return 'Invalid deployment configuration. Check the parameters and try again.';
    case 500:
      return 'Kubernetes API server error. Try again later.';
    case 502:
    case 503:
    case 504:
      return 'Kubernetes API server is temporarily unavailable. Try again later.';
    default:
      return `Request failed with status ${statusCode}`;
  }
}

/** Decode legacy and generated-client Kubernetes Status bodies. */
function parseK8sErrorBody(error: K8sApiError): K8sErrorBody | string | undefined {
  const raw = error.body ?? error.response?.body;
  if (typeof raw !== 'string') return raw;
  try {
    const parsed: unknown = JSON.parse(raw);
    return parsed && typeof parsed === 'object' && !Array.isArray(parsed)
      ? parsed as K8sErrorBody : raw;
  } catch {
    return raw;
  }
}

/** Preserve HTTP errors from both legacy clients and the generated ApiException. */
export function getK8sStatusCode(error: unknown): number | undefined {
  if (!error || typeof error !== 'object') return undefined;
  const apiError = error as K8sApiError;
  const body = parseK8sErrorBody(apiError);
  const candidates = [apiError.statusCode, apiError.response?.statusCode, apiError.code,
    typeof body === 'object' ? body?.code : undefined];
  return candidates.find((value): value is number => typeof value === 'number'
    && Number.isInteger(value) && value >= 400 && value < 600);
}

export function getK8sErrorStatusCode(error: unknown): number {
  return getK8sStatusCode(error) ?? 500;
}

/**
 * Log detailed K8s error information and return user-friendly message
 */
export function handleK8sError(error: unknown, context: Record<string, unknown> = {}): {
  message: string;
  statusCode: number;
} {
  const message = extractK8sErrorMessage(error);
  const statusCode = getK8sErrorStatusCode(error);

  // Log full error details for debugging
  logger.error(
    {
      ...context,
      errorMessage: message,
      statusCode,
      rawError: error instanceof Error ? { message: error.message, stack: error.stack } : error,
    },
    `Kubernetes API error: ${message}`
  );

  return { message, statusCode };
}
