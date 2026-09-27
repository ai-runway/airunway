import { readFileSync } from 'fs';
import { load } from 'js-yaml';
import { describe, expect, test } from 'bun:test';

type RbacRule = {
  apiGroups: string[];
  resources: string[];
  verbs: string[];
};

type RbacRole = {
  rules: RbacRule[];
};

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

function isStringArray(value: unknown): value is string[] {
  return Array.isArray(value) && value.every((item: unknown) => typeof item === 'string');
}

function isRbacRule(value: unknown): value is RbacRule {
  return isRecord(value)
    && isStringArray(value.apiGroups)
    && isStringArray(value.resources)
    && isStringArray(value.verbs);
}

function parseRbacRole(source: string): RbacRole {
  const document: unknown = load(source);
  if (!isRecord(document) || !Array.isArray(document.rules)) {
    throw new Error('Dashboard ClusterRole must contain a rules array');
  }

  const rules = document.rules.map((rule: unknown) => {
    if (!isRbacRule(rule)) {
      throw new Error('Dashboard ClusterRole contains an invalid RBAC rule');
    }
    return rule;
  });

  return { rules };
}

const dashboardRole = parseRbacRole(
  readFileSync(new URL('../config/rbac/role.yaml', import.meta.url), 'utf8'),
);

describe('Dashboard RBAC', () => {
  test('can list KAITO resources to verify CRD preservation during uninstall', () => {
    const kaitoListRule = dashboardRole.rules.find((rule) => (
      rule.apiGroups.includes('kaito.sh')
      && ['workspaces', 'inferencesets'].every((resource) => rule.resources.includes(resource))
      && rule.verbs.includes('list')
    ));

    expect(kaitoListRule).toBeDefined();
    expect(kaitoListRule?.verbs).toEqual(['list']);
  });
});
