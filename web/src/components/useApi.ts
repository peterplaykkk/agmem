// 通用 fetch hook：五态（loading/data/empty/error/retry），AbortController 取消过期请求。
import { useCallback, useEffect, useRef, useState } from 'react';
import { ApiError } from '../api';

export interface ApiState<T> {
  loading: boolean;
  data: T | null;
  error: ApiError | null;
  retry: () => void;
  reloading: boolean;
}

export function useApi<T>(
  fetcher: (signal: AbortSignal) => Promise<T>,
  deps: readonly unknown[],
  options?: { pollMs?: number },
): ApiState<T> {
  const [data, setData] = useState<T | null>(null);
  const [error, setError] = useState<ApiError | null>(null);
  const [loading, setLoading] = useState(true);
  const [reloading, setReloading] = useState(false);
  const [tick, setTick] = useState(0);
  // 连续失败计数：轮询退避 2s/4s/8s，3 次仍失败才停（避免一次错误后永久停摆）。
  const failRef = useRef(0);

  const retry = useCallback(() => setTick((t) => t + 1), []);

  useEffect(() => {
    const ctrl = new AbortController();
    let first = true;
    if (data != null || error != null) {
      first = false;
      setReloading(true);
    }
    setLoading(first);
    fetcher(ctrl.signal)
      .then((d) => {
        if (ctrl.signal.aborted) return;
        failRef.current = 0;
        setData(d);
        setError(null);
      })
      .catch((e: unknown) => {
        if (ctrl.signal.aborted || (e instanceof DOMException && e.name === 'AbortError')) return;
        failRef.current += 1;
        setData(null);
        setError(
          e instanceof ApiError ? e : new ApiError(0, e instanceof Error ? e.message : String(e)),
        );
      })
      .finally(() => {
        if (ctrl.signal.aborted) return;
        setLoading(false);
        setReloading(false);
      });
    return () => ctrl.abort();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [...deps, tick]);

  // 轮询：成功后按 pollMs 周期；失败按 2/4/8s 退避，连续 3 次失败停止。
  useEffect(() => {
    const ms = options?.pollMs;
    if (!ms) return;
    let timer: number | undefined;
    const schedule = () => {
      const fails = failRef.current;
      if (fails >= 3) return;
      const delay = fails === 0 ? ms : Math.min(ms * 2 ** fails, 8000);
      timer = window.setTimeout(() => {
        if (document.visibilityState === 'visible') retry();
        else schedule();
      }, delay);
    };
    schedule();
    return () => window.clearTimeout(timer);
    // tick/data/error 变化（每次取数完成）后重新调度下一次轮询。
  }, [options?.pollMs, retry, tick, data, error]);

  return { loading, data, error, retry, reloading };
}
