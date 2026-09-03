/**
 * Logs Viewer Component
 *
 * Displays pod logs with pod selection and auto-scroll.
 */

import type { PodLogsResponse, PodStatus } from '@airunway/shared';
import { useEffect, useRef } from 'react';

interface LogsViewerProps {
  logs: PodLogsResponse | null;
  pods: PodStatus[];
  selectedPod: string | null;
  onSelectPod: (podName: string) => void;
  onRefresh?: () => void;
}

export function LogsViewer({ logs, pods, selectedPod, onSelectPod, onRefresh }: LogsViewerProps) {
  const logsContainerRef = useRef<HTMLDivElement>(null);

  // Auto-scroll to bottom when logs change
  useEffect(() => {
    if (logsContainerRef.current) {
      logsContainerRef.current.scrollTop = logsContainerRef.current.scrollHeight;
    }
  }, [logs]);

  return (
    <div>
      {/* Controls */}
      <div style={{ display: 'flex', gap: '12px', marginBottom: '16px', alignItems: 'center' }}>
        <div>
          <label htmlFor="logs-pod" style={{ marginRight: '8px', fontSize: '14px' }}>Pod:</label>
          <select
            id="logs-pod"
            value={selectedPod || ''}
            onChange={(e) => onSelectPod(e.target.value)}
            style={{
              padding: '6px 12px',
              border: '1px solid rgba(128, 128, 128, 0.3)',
              borderRadius: '4px',
              fontSize: '14px',
              backgroundColor: 'transparent',
              color: 'inherit',
            }}
          >
            <option value="">All Pods</option>
            {pods.map((pod) => (
              <option key={pod.name} value={pod.name}>
                {pod.name}
              </option>
            ))}
          </select>
        </div>

        {onRefresh && (
          <button
            onClick={onRefresh}
            style={{
              padding: '6px 12px',
              backgroundColor: 'transparent',
              border: '1px solid rgba(128, 128, 128, 0.3)',
              borderRadius: '4px',
              cursor: 'pointer',
              fontSize: '13px',
              color: 'inherit',
            }}
          >
            Refresh
          </button>
        )}
      </div>

      {/* Logs container */}
      <div
        ref={logsContainerRef}
        style={{
          backgroundColor: '#1e1e1e',
          color: '#d4d4d4',
          fontFamily: 'Monaco, Menlo, Consolas, monospace',
          fontSize: '12px',
          lineHeight: '1.5',
          padding: '16px',
          borderRadius: '8px',
          height: '400px',
          overflow: 'auto',
          whiteSpace: 'pre-wrap',
          wordBreak: 'break-all',
        }}
      >
        {!logs || !logs.logs ? (
          <div style={{ color: '#808080', fontStyle: 'italic' }}>
            No logs available. The pod may still be starting up.
          </div>
        ) : logs.logs.length === 0 ? (
          <div style={{ color: '#808080', fontStyle: 'italic' }}>
            No log entries found.
          </div>
        ) : (
          <pre style={{ margin: 0, fontFamily: 'inherit' }}>
            {logs.logs}
          </pre>
        )}
      </div>

      {/* Log info */}
      {logs && (
        <div style={{ fontSize: '12px', opacity: 0.6, marginTop: '8px', display: 'flex', gap: '16px' }}>
          {logs.podName && <span>Pod: {logs.podName}</span>}
          {logs.container && <span>Container: {logs.container}</span>}
          {logs.logs && <span>Lines: {logs.logs.split('\n').length}</span>}
        </div>
      )}
    </div>
  );
}
