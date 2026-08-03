import { useState, useEffect } from 'react'
import { DollarSign, Info, AlertCircle, Loader2, Zap, ChevronDown } from 'lucide-react'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Badge } from '@/components/ui/badge'
import { cn } from '@/lib/utils'
import {
  Tooltip,
  TooltipContent,
  TooltipProvider,
  TooltipTrigger,
} from '@/components/ui/tooltip'
import type { NodePoolInfo, NodePoolCostEstimate } from '@/lib/api'
import { costsApi } from '@/lib/api'

// No more static pricing - all pricing comes from cloud provider APIs via the backend

interface CostEstimateProps {
  /** Node pools with GPU info */
  nodePools?: NodePoolInfo[]
  /** Number of GPUs per replica (ignored for CPU) */
  gpuCount: number
  /** Number of replicas */
  replicas: number
  /** Compute type: 'gpu' or 'cpu' */
  computeType?: 'gpu' | 'cpu'
  /** Show compact version */
  compact?: boolean
  /** Additional CSS class */
  className?: string
}

/**
 * Display cost estimates for GPU deployments
 * Fetches real-time pricing from cloud provider APIs
 */
export function CostEstimate({
  nodePools,
  gpuCount,
  replicas,
  computeType = 'gpu',
  compact = false,
  className = '',
}: CostEstimateProps) {
  const [nodePoolCosts, setNodePoolCosts] = useState<NodePoolCostEstimate[]>([])
  const [isLoading, setIsLoading] = useState(false)
  const [pricingSource, setPricingSource] = useState<string>('')
  const [pricingError, setPricingError] = useState<string | null>(null)
  const [unsupportedProvider, setUnsupportedProvider] = useState<boolean>(false)
  const [isExpanded, setIsExpanded] = useState(false)

  // Fetch real-time pricing from backend API
  useEffect(() => {
    // For CPU, we don't need nodePools - the backend will fetch them
    // For GPU, we need nodePools to be present
    if (computeType === 'gpu' && (!nodePools || nodePools.length === 0)) return

    const fetchPricing = async () => {
      setIsLoading(true)
      setPricingError(null)
      setUnsupportedProvider(false)
      try {
        const response = await costsApi.getNodePoolCosts(gpuCount, replicas, computeType)
        if (response.success && response.nodePoolCosts) {
          setNodePoolCosts(response.nodePoolCosts)
          setPricingSource(response.pricingSource || 'realtime')
          
          // Check if any pool has real-time pricing - if none do, provider is unsupported
          const hasRealtimePricing = response.nodePoolCosts.some(
            (pool) => pool.realtimePricing && pool.realtimePricing.hourlyPrice > 0
          )
          if (!hasRealtimePricing && response.nodePoolCosts.length > 0) {
            setUnsupportedProvider(true)
          }
        }
      } catch {
        // Error is expected when cloud pricing API is unavailable
        setPricingError('Unable to fetch pricing from cloud provider API')
        setNodePoolCosts([])
      } finally {
        setIsLoading(false)
      }
    }

    fetchPricing()
  }, [nodePools, gpuCount, replicas, computeType])

  // Show unsupported provider message
  if (unsupportedProvider) {
    return (
      <div className={`flex flex-col gap-1 text-sm text-muted-foreground ${className}`}>
        <div className="flex items-center gap-2">
          <Info className="h-4 w-4 text-blue-500" />
          <span>Real-time pricing is not yet available for your cloud provider.</span>
        </div>
        <p className="text-xs text-muted-foreground ml-6">
          AWS and GCP pricing support coming soon.
        </p>
      </div>
    )
  }

  // Show error state
  if (pricingError) {
    return (
      <div className={`flex items-center gap-2 text-sm text-muted-foreground ${className}`}>
        <AlertCircle className="h-4 w-4 text-yellow-500" />
        <span>{pricingError}</span>
      </div>
    )
  }

  // If no pools with GPU info (and not CPU mode), show nothing
  if (computeType === 'gpu' && (!nodePools || nodePools.length === 0)) {
    return null
  }

  // Format currency
  const formatCurrency = (amount: number) => {
    return new Intl.NumberFormat('en-US', {
      style: 'currency',
      currency: 'USD',
      minimumFractionDigits: 2,
      maximumFractionDigits: 2,
    }).format(amount)
  }

  // Helper to get best pricing (realtime preferred)
  const getBestPricing = (poolCost: NodePoolCostEstimate) => {
    if (poolCost.realtimePricing) {
      return {
        hourly: poolCost.realtimePricing.hourlyPrice,
        monthly: poolCost.realtimePricing.monthlyPrice,
        source: poolCost.realtimePricing.source,
        instanceType: poolCost.realtimePricing.instanceType,
        region: poolCost.realtimePricing.region,
      }
    }
    return {
      hourly: poolCost.costBreakdown.estimate.hourly,
      monthly: poolCost.costBreakdown.estimate.monthly,
      source: 'static' as const,
      instanceType: undefined,
      region: undefined,
    }
  }

  // Compact view - just show primary estimate
  if (compact) {
    const primaryCost = nodePoolCosts[0]
    if (!primaryCost) return null

    const pricing = getBestPricing(primaryCost)
    const isRealtime = pricing.source === 'realtime' || pricing.source === 'cached'

    return (
      <TooltipProvider>
        <Tooltip>
          <TooltipTrigger asChild>
            <div className={`flex items-center gap-1.5 text-sm text-muted-foreground ${className}`}>
              {isRealtime && <Zap className="h-4 w-4 text-green-500" />}
              {!isRealtime && <DollarSign className="h-4 w-4" />}
              <span>
                ~{formatCurrency(pricing.hourly)}/hr
              </span>
              <span className="text-xs">
                ({formatCurrency(pricing.monthly)}/mo)
              </span>
              {isLoading && <Loader2 className="h-3 w-3 animate-spin" />}
            </div>
          </TooltipTrigger>
          <TooltipContent side="bottom" className="max-w-xs">
            <div className="space-y-1 text-xs">
              <p className="font-medium">
                {primaryCost.availableGpus} × {primaryCost.gpuModel}
              </p>
              {pricing.instanceType && (
                <p className="text-green-600 dark:text-green-400">
                  {pricing.instanceType} ({pricing.region})
                </p>
              )}
              <p>
                {isRealtime 
                  ? 'Real-time Azure pricing' 
                  : 'Based on average cloud provider rates'}
              </p>
              <p className="text-muted-foreground">Spot instances can be 60-80% cheaper</p>
            </div>
          </TooltipContent>
        </Tooltip>
      </TooltipProvider>
    )
  }

  // Full view - show per-pool breakdown
  return (
    <Card className={className} data-testid="cost-estimate-card">
      <CardHeader
        className="cursor-pointer select-none"
        onClick={() => setIsExpanded(!isExpanded)}
      >
        <div className="flex items-center justify-between">
          <CardTitle className="flex items-center gap-2">
            Estimated Cost
            {pricingSource.includes('realtime') && (
              <Badge variant="secondary" className="text-xs font-normal gap-1">
                <Zap className="h-3 w-3" />
                Live
              </Badge>
            )}
            {isLoading && <Loader2 className="h-3.5 w-3.5 animate-spin" />}
            <TooltipProvider>
              <Tooltip>
                <TooltipTrigger asChild>
                  <span className="cursor-help" onClick={(e) => e.stopPropagation()}>
                    <Info className="h-3.5 w-3.5 text-muted-foreground" />
                  </span>
                </TooltipTrigger>
                <TooltipContent side="right" className="max-w-xs">
                  <p className="text-xs">
                    {pricingSource.includes('realtime')
                      ? 'Real-time pricing from Azure Retail Prices API. Reflects current on-demand VM costs.'
                      : 'Estimates based on average on-demand cloud rates.'}
                    {' '}Actual costs vary by commitment level. Spot instances can save 60-80%.
                  </p>
                </TooltipContent>
              </Tooltip>
            </TooltipProvider>
          </CardTitle>
          <ChevronDown
            className={cn(
              "h-5 w-5 text-muted-foreground transition-transform duration-200 ease-out",
              isExpanded && "rotate-180"
            )}
          />
        </div>
      </CardHeader>

      {/* Smooth accordion animation */}
      <div
        className={cn(
          "grid transition-all duration-300 ease-out",
          isExpanded ? "grid-rows-[1fr] opacity-100" : "grid-rows-[0fr] opacity-0"
        )}
      >
        <div className="overflow-hidden">
          <CardContent className="space-y-4 pt-0">
            {isLoading && nodePoolCosts.length === 0 ? (
              <div className="flex items-center gap-2 text-sm text-muted-foreground py-2">
                <Loader2 className="h-4 w-4 animate-spin" />
                <span>Loading pricing...</span>
              </div>
            ) : nodePoolCosts.length === 0 ? (
              <div className="text-sm text-muted-foreground py-2">
                No pricing data available
              </div>
            ) : (
              <>
        {nodePoolCosts.map((poolCost) => {
          const pricing = getBestPricing(poolCost)
          const isRealtime = pricing.source === 'realtime' || pricing.source === 'cached'
          const cost = poolCost.costBreakdown

          return (
            <div key={poolCost.poolName} className="space-y-2">
              <div className="flex items-center justify-between">
                <span className="text-sm font-medium">{poolCost.poolName}</span>
                <div className="flex items-center gap-2">
                  <Badge variant="outline" className="text-xs">
                    {poolCost.gpuModel}
                  </Badge>
                </div>
              </div>

              {/* Instance type info for realtime pricing */}
              {pricing.instanceType && (
                <div className="text-xs text-muted-foreground flex items-center gap-1">
                  <Zap className="h-3 w-3 text-green-500" />
                  <span>{pricing.instanceType}</span>
                  {pricing.region && <span className="text-muted-foreground">({pricing.region})</span>}
                </div>
              )}

              <div className="grid grid-cols-2 gap-4">
                <div className="space-y-1">
                  <p className="text-xs text-muted-foreground">Hourly</p>
                  <p className="text-lg font-semibold" data-testid="hourly-cost">
                    {formatCurrency(pricing.hourly)}
                  </p>
                </div>
                <div className="space-y-1">
                  <p className="text-xs text-muted-foreground">Monthly (24/7)</p>
                  <p className="text-lg font-semibold" data-testid="monthly-cost">
                    {formatCurrency(pricing.monthly)}
                  </p>
                </div>
              </div>

              {/* Show static provider breakdown only if no realtime pricing */}
              {!isRealtime && cost.byProvider && cost.byProvider.length > 0 && (
                <div className="pt-2 border-t">
                  <p className="text-xs text-muted-foreground mb-1.5">By Provider (Static Estimates)</p>
                  <div className="flex flex-wrap gap-2">
                    {cost.byProvider.map((provider) => (
                      <Badge key={provider.provider} variant="secondary" className="text-xs font-normal">
                        {provider.provider.toUpperCase()}: {formatCurrency(provider.hourly)}/hr
                      </Badge>
                    ))}
                  </div>
                </div>
              )}

              {/* Low confidence warning */}
              {!isRealtime && cost.estimate.confidence === 'low' && (
                <div className="flex items-center gap-1.5 text-xs text-amber-600 dark:text-amber-400">
                  <AlertCircle className="h-3 w-3" />
                  <span>Limited pricing data available for this GPU</span>
                </div>
              )}
            </div>
          )
        })}

              </>
            )}
          </CardContent>
        </div>
      </div>
    </Card>
  )
}

export default CostEstimate
