import { ExternalLink } from 'lucide-react';
import { Alert, AlertDescription, AlertTitle } from '@/components/ui/alert';
import { Button } from '@/components/ui/button';
import type { AutoscalerDetectionResult } from '@airunway/shared';

interface AutoscalerGuidanceProps {
  autoscaler?: AutoscalerDetectionResult;
  variant?: 'default' | 'inline';
  className?: string;
}

export function AutoscalerGuidance({ autoscaler, variant = 'default', className }: AutoscalerGuidanceProps) {
  if (!autoscaler) {
    return null;
  }

  const getGuidanceContent = () => {
    if (autoscaler.detected && autoscaler.healthy) {
      return {
        title: 'Autoscaling Enabled',
        description: `Your cluster has ${autoscaler.type === 'aks-managed' ? 'AKS managed autoscaling' : 'cluster autoscaler'} enabled and healthy. The cluster will automatically provision GPU nodes when needed.`,
        links: [
          {
            label: 'Verify Configuration',
            href: 'https://github.com/ai-runway/airunway/blob/main/docs/azure-autoscaling.md#verification',
          },
        ],
      };
    }

    if (autoscaler.detected && !autoscaler.healthy) {
      return {
        title: 'Autoscaler Unhealthy',
        description: `Autoscaler is detected but appears unhealthy: ${autoscaler.message}. Scale-up may not work as expected.`,
        links: [
          {
            label: 'Troubleshooting Guide',
            href: 'https://github.com/ai-runway/airunway/blob/main/docs/azure-autoscaling.md#troubleshooting',
          },
        ],
      };
    }

    // Not detected - provide setup guidance
    return {
      title: 'Autoscaling Not Detected',
      description: 'Enable cluster autoscaling to automatically provision GPU nodes when deployments require more resources than available.',
      links: [
        {
          label: 'Setup Guide (AKS)',
          href: 'https://github.com/ai-runway/airunway/blob/main/docs/azure-autoscaling.md#option-1-aks-managed-autoscaler-recommended',
        },
      ],
    };
  };

  const content = getGuidanceContent();

  if (variant === 'inline') {
    return (
      <div className={className}>
        <div className="flex items-center gap-2 flex-wrap">
          {content.links.map((link) => (
            <a
              key={link.label}
              href={link.href}
              target="_blank"
              rel="noopener noreferrer"
              className="text-sm text-primary hover:underline flex items-center gap-1"
            >
              {link.label}
              <ExternalLink className="h-3 w-3" />
            </a>
          ))}
        </div>
      </div>
    );
  }

  return (
    <Alert className={className}>
      <AlertTitle>{content.title}</AlertTitle>
      <AlertDescription className="space-y-3">
        <p>{content.description}</p>
        <div className="flex flex-wrap gap-2">
          {content.links.map((link) => (
            <a
              key={link.label}
              href={link.href}
              target="_blank"
              rel="noopener noreferrer"
            >
              <Button variant="outline" size="sm">
                {link.label}
                <ExternalLink className="ml-2 h-3 w-3" />
              </Button>
            </a>
          ))}
        </div>
      </AlertDescription>
    </Alert>
  );
}
