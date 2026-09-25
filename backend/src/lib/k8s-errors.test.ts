import { describe, expect, it } from 'bun:test';
import { extractK8sErrorMessage, getK8sErrorStatusCode, handleK8sError } from './k8s-errors';

describe('extractK8sErrorMessage', () => {
  it('returns message from standard Error', () => {
    const error = new Error('Something went wrong');
    expect(extractK8sErrorMessage(error)).toBe('Something went wrong');
  });

  it('returns "Unknown error occurred" for null/undefined', () => {
    expect(extractK8sErrorMessage(null)).toBe('Unknown error occurred');
    expect(extractK8sErrorMessage(undefined)).toBe('Unknown error occurred');
  });

  it('extracts message from K8s Status response body', () => {
    const error = {
      response: {
        statusCode: 403,
        body: {
          kind: 'Status',
          apiVersion: 'v1',
          status: 'Failure',
          message: 'workspaces.kaito.sh is forbidden: User "system:serviceaccount:default:airunway" cannot create resource "workspaces" in API group "kaito.sh"',
          reason: 'Forbidden',
          code: 403,
        },
      },
    };
    
    const message = extractK8sErrorMessage(error);
    expect(message).toContain('workspaces.kaito.sh is forbidden');
  });

  it('extracts field-level causes from K8s validation errors', () => {
    const error = {
      response: {
        statusCode: 422,
        body: {
          kind: 'Status',
          apiVersion: 'v1',
          status: 'Failure',
          message: 'Workspace "test" is invalid',
          reason: 'Invalid',
          details: {
            name: 'test',
            kind: 'Workspace',
            causes: [
              { field: 'spec.resource.instanceType', message: 'is not supported', reason: 'FieldValueNotSupported' },
              { field: 'spec.resource.count', message: 'must be at least 1', reason: 'FieldValueInvalid' },
            ],
          },
          code: 422,
        },
      },
    };
    
    const message = extractK8sErrorMessage(error);
    expect(message).toContain('Workspace "test" is invalid');
    expect(message).toContain('spec.resource.instanceType: is not supported');
    expect(message).toContain('spec.resource.count: must be at least 1');
  });

  it('parses JSON string body', () => {
    const error = {
      response: {
        statusCode: 409,
        body: JSON.stringify({
          kind: 'Status',
          message: 'workspaces.kaito.sh "my-model" already exists',
          reason: 'AlreadyExists',
          code: 409,
        }),
      },
    };
    
    const message = extractK8sErrorMessage(error);
    expect(message).toContain('already exists');
  });

  it('returns plain string body if not JSON', () => {
    const error = {
      response: {
        statusCode: 500,
        body: 'Internal server error occurred',
      },
    };
    
    expect(extractK8sErrorMessage(error)).toBe('Internal server error occurred');
  });

  it('handles "HTTP request failed" with status code context', () => {
    const error = {
      message: 'HTTP request failed',
      response: { statusCode: 403 },
    };
    
    const message = extractK8sErrorMessage(error);
    expect(message).toContain('Permission denied');
  });

  it('provides user-friendly message for 404 status', () => {
    const error = {
      message: 'HTTP request failed',
      statusCode: 404,
    };
    
    const message = extractK8sErrorMessage(error);
    expect(message).toContain('not found');
  });

  it('provides user-friendly message for 409 conflict', () => {
    const error = {
      message: 'HTTP request failed',
      statusCode: 409,
    };
    
    const message = extractK8sErrorMessage(error);
    expect(message).toContain('already exists');
  });
});

describe('getK8sErrorStatusCode', () => {
  it('extracts status code from statusCode property', () => {
    expect(getK8sErrorStatusCode({ statusCode: 403 })).toBe(403);
  });

  it('extracts status code from response.statusCode', () => {
    expect(getK8sErrorStatusCode({ response: { statusCode: 404 } })).toBe(404);
  });

  it('extracts numeric status codes from the Kubernetes client code property', () => {
    expect(getK8sErrorStatusCode({ code: 429 })).toBe(429);
    expect(getK8sErrorStatusCode({ code: 503 })).toBe(503);
  });

  it('extracts status code from body.code', () => {
    expect(getK8sErrorStatusCode({ body: { code: 422 } })).toBe(422);
  });

  it('returns 500 for unknown errors', () => {
    expect(getK8sErrorStatusCode({})).toBe(500);
    expect(getK8sErrorStatusCode(null)).toBe(500);
  });
});

describe('handleK8sError', () => {
  it('returns message and statusCode', () => {
    const error = {
      statusCode: 403,
      response: {
        body: {
          message: 'Forbidden: cannot create workspaces',
          code: 403,
        },
      },
    };
    
    const result = handleK8sError(error, { operation: 'createDeployment' });
    expect(result.message).toContain('Forbidden');
    expect(result.statusCode).toBe(403);
  });

  it('preserves numeric Kubernetes client status codes and friendly messages', () => {
    const result = handleK8sError({ code: 503, message: 'HTTP request failed' });

    expect(result.message).toContain('temporarily unavailable');
    expect(result.statusCode).toBe(503);
  });
});
