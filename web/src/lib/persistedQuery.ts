import { useEffect } from 'react';
import { useQuery, type QueryKey } from '@tanstack/react-query';

// Pages backed by slow third-party lookups (Ticketmaster, Last.fm) or data that
// only changes on discography syncs keep their last response in localStorage
// and are treated as fresh for a week, so revisits render instantly without
// refetching. Each page's Refresh button forces a reload.
export const WEEK_MS = 7 * 24 * 60 * 60 * 1000;
const PREFIX = 'crate:cache:';

interface Stored<T> {
  data: T;
  updatedAt: number;
}

function readStored<T>(name: string): Stored<T> | undefined {
  try {
    const raw = localStorage.getItem(PREFIX + name);
    if (!raw) return undefined;
    const stored = JSON.parse(raw) as Stored<T>;
    if (typeof stored?.updatedAt !== 'number' || Date.now() - stored.updatedAt > WEEK_MS) return undefined;
    return stored;
  } catch {
    return undefined;
  }
}

export function usePersistedQuery<T>(name: string, queryKey: QueryKey, queryFn: () => Promise<T>) {
  const query = useQuery({
    queryKey,
    queryFn,
    initialData: () => readStored<T>(name)?.data,
    initialDataUpdatedAt: () => readStored<T>(name)?.updatedAt,
    staleTime: WEEK_MS,
    gcTime: WEEK_MS,
    refetchOnWindowFocus: false,
  });

  const { data, dataUpdatedAt } = query;
  useEffect(() => {
    if (data === undefined || !dataUpdatedAt) return;
    try {
      localStorage.setItem(PREFIX + name, JSON.stringify({ data, updatedAt: dataUpdatedAt }));
    } catch {
      // storage full or unavailable — in-memory cache still works
    }
  }, [name, data, dataUpdatedAt]);

  return query;
}
