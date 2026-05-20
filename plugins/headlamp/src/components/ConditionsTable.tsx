/**
 * Conditions Table Component
 *
 * Displays Kubernetes-style conditions in a table with color-coded status.
 */

import {
  SimpleTable,
  StatusLabel,
  StatusLabelProps,
} from '@kinvolk/headlamp-plugin/lib/CommonComponents';
import type { Condition } from '@airunway/shared';

interface ConditionsTableProps {
  conditions: Condition[];
}

function getConditionStatusColor(status: Condition['status']): StatusLabelProps['status'] {
  switch (status) {
    case 'True':
      return 'success';
    case 'False':
      return 'error';
    case 'Unknown':
      return 'warning';
    default:
      return '';
  }
}

function formatTransitionTime(time?: string): string {
  if (!time) {
    return '-';
  }
  return new Date(time).toLocaleString();
}

export function ConditionsTable({ conditions }: ConditionsTableProps) {
  if (conditions.length === 0) {
    return (
      <div style={{ textAlign: 'center', padding: '24px', opacity: 0.6 }}>
        No conditions reported
      </div>
    );
  }

  return (
    <SimpleTable
      columns={[
        { label: 'Type', getter: (c: Condition) => c.type },
        {
          label: 'Status',
          getter: (c: Condition) => (
            <StatusLabel status={getConditionStatusColor(c.status)}>
              {c.status}
            </StatusLabel>
          ),
        },
        { label: 'Reason', getter: (c: Condition) => c.reason || '-' },
        { label: 'Message', getter: (c: Condition) => c.message || '-' },
        {
          label: 'Last Transition',
          getter: (c: Condition) => formatTransitionTime(c.lastTransitionTime),
        },
      ]}
      data={conditions}
    />
  );
}
