import { Info } from 'lucide-react'
import { Tooltip, TooltipContent, TooltipProvider, TooltipTrigger } from './tooltip'

export function InfoHint({ text }: { text: string }) {
  return <TooltipProvider><Tooltip><TooltipTrigger asChild>
    <button type="button" aria-label={text} className="inline-flex text-muted-foreground"><Info className="h-4 w-4" /></button>
  </TooltipTrigger><TooltipContent className="max-w-xs">{text}</TooltipContent></Tooltip></TooltipProvider>
}
