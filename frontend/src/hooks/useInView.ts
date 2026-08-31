import { useEffect, useRef, useState, type RefObject } from 'react'

/**
 * Returns a ref to attach to an element and a boolean that flips to true the
 * first time the element scrolls into the viewport. Used to defer expensive
 * per-card work (e.g. throughput estimates that fetch HF config.json) until a
 * card is actually visible. Once seen, it stays true (no flickering).
 */
export function useInView<T extends Element = HTMLDivElement>(
  options?: IntersectionObserverInit
): { ref: RefObject<T>; inView: boolean } {
  const ref = useRef<T>(null!)
  const [inView, setInView] = useState(false)

  useEffect(() => {
    const element = ref.current
    if (!element || inView) return
    if (typeof IntersectionObserver === 'undefined') {
      // Fallback for environments without IntersectionObserver (e.g. jsdom).
      setInView(true)
      return
    }

    const observer = new IntersectionObserver((entries) => {
      if (entries.some((entry) => entry.isIntersecting)) {
        setInView(true)
        observer.disconnect()
      }
    }, options ?? { rootMargin: '100px' })

    observer.observe(element)
    return () => observer.disconnect()
  }, [inView, options])

  return { ref, inView }
}
