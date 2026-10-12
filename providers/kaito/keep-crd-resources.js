#!/usr/bin/env node
'use strict';

const fs = require('node:fs');
const input = fs.readFileSync(0, 'utf8');
const documents = input.split(/(?=^---\s*$)/m);

function inspectCrd(document) {
  const lines = document.split(/\r?\n/);
  const kindIndex = lines.findIndex((line) => /^kind:\s*CustomResourceDefinition\s*$/.test(line));
  if (kindIndex < 0) return null;

  const metadataIndex = lines.findIndex((line, index) => index > kindIndex && /^metadata:\s*$/.test(line));
  if (metadataIndex < 0) throw new Error('CustomResourceDefinition has no metadata map');
  const metadataEnd = lines.findIndex((line, index) => index > metadataIndex && /^[^\s]/.test(line));
  const end = metadataEnd < 0 ? lines.length : metadataEnd;
  const nameLine = lines.slice(metadataIndex + 1, end).find((line) => /^  name:\s*\S/.test(line));
  if (!nameLine) throw new Error('CustomResourceDefinition has no metadata.name');
  const name = nameLine.replace(/^  name:\s*/, '').replace(/\s+#.*$/, '').replace(/^['"]|['"]$/g, '');

  const metadataLines = lines.slice(metadataIndex + 1, end);
  const annotationIndex = metadataLines.findIndex((line) => /^  annotations:\s*(?:#.*)?$/.test(line));
  const hasAnnotations = metadataLines.some((line) => /^  annotations:/.test(line));
  if (hasAnnotations && annotationIndex < 0) {
    throw new Error('CustomResourceDefinition annotations are not a block map');
  }

  const annotationEnd = annotationIndex < 0
    ? -1
    : metadataLines.findIndex((line, index) => (
      index > annotationIndex && line.trim() !== '' && /^  \S/.test(line)
    ));
  const annotationsStop = annotationEnd < 0 ? metadataLines.length : annotationEnd;
  const policyLines = annotationIndex < 0 ? [] : metadataLines
    .map((line, index) => ({ line, index }))
    .filter(({ line, index }) => (
      index > annotationIndex
      && index < annotationsStop
      && /^    helm\.sh\/resource-policy\s*:/.test(line)
    ));
  if (policyLines.length > 1) {
    throw new Error('CustomResourceDefinition has duplicate resource-policy annotations');
  }
  const policy = policyLines.length === 0
    ? undefined
    : policyLines[0].line
      .replace(/^    helm\.sh\/resource-policy\s*:\s*/, '')
      .replace(/\s+#.*$/, '')
      .replace(/^['"]|['"]$/g, '');
  return { lines, name, metadataIndex, annotationIndex, policyLines, policy };
}

function keepCrd(document) {
  const crd = inspectCrd(document);
  if (!crd) return document;
  if (crd.policyLines.length > 0) {
    const policyLine = crd.policyLines[0].line;
    crd.lines[crd.lines.indexOf(policyLine)] = '    helm.sh/resource-policy: keep';
  } else if (crd.annotationIndex >= 0) {
    const annotationsLine = crd.lines.findIndex((line, index) => (
      index > crd.metadataIndex && /^  annotations:\s*(?:#.*)?$/.test(line)
    ));
    crd.lines.splice(annotationsLine + 1, 0, '    helm.sh/resource-policy: keep');
  } else {
    crd.lines.splice(crd.metadataIndex + 1, 0, '  annotations:', '    helm.sh/resource-policy: keep');
  }
  return crd.lines.join('\n');
}

if (process.argv[2] === '--check-kept') {
  try {
    const resources = new Map();
    for (const document of documents) {
      const crd = inspectCrd(document);
      if (!crd) continue;
      if (resources.has(crd.name)) throw new Error('Duplicate CustomResourceDefinition ' + crd.name);
      resources.set(crd.name, crd.policy);
    }
    const errors = [];
    for (const [name, policy] of resources) {
      if (policy !== 'keep') errors.push(name + ' is missing helm.sh/resource-policy=keep');
    }
    const expectedNames = process.argv.slice(3);
    if (expectedNames.length === 0) errors.push('no expected CRD names were provided');
    for (const name of expectedNames) {
      if (!resources.has(name)) errors.push(name + ' is missing from the Helm release manifest');
    }
    if (errors.length > 0) throw new Error(errors.join('; '));
    process.stdout.write('Verified ' + resources.size + ' retained CRD resource(s).\n');
  } catch (error) {
    process.stderr.write('Cannot verify retained CRDs: ' + error.message + '\n');
    process.exitCode = 1;
  }
} else {
  try {
    process.stdout.write(documents.map(keepCrd).join(''));
  } catch (error) {
    process.stderr.write('Cannot retain CRD resources: ' + error.message + '\n');
    process.exitCode = 1;
  }
}
